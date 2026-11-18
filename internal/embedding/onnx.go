//go:build embeddings_onnx

package embedding

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/zzet/gortex/internal/platform"
)

const (
	onnxMaxSeqLen = 128
	onnxDims      = 384
	clsTokenID    = 101
	sepTokenID    = 102
	unkTokenID    = 100
	padTokenID    = 0
)

// ONNXProvider uses GTE-small via ONNX Runtime for high-quality embeddings.
// Creates a single session with fixed-size input tensors for fast reuse.
type ONNXProvider struct {
	vocab   map[string]int64
	session *ort.AdvancedSession

	// Pre-allocated tensors (fixed shape: 1 × onnxMaxSeqLen).
	inputIDs      *ort.Tensor[int64]
	attentionMask *ort.Tensor[int64]
	tokenTypeIDs  *ort.Tensor[int64]
	output        *ort.Tensor[float32]

	mu sync.Mutex
}

func newONNXProvider() (_ Provider, err error) {
	modelDir := findONNXModelDir()
	if modelDir == "" {
		return nil, fmt.Errorf("ONNX model not found; this backend never auto-downloads — manually place model.onnx + vocab.txt in ~/.gortex/models/gte-small/ (plus `brew install onnxruntime` or the distro equivalent); see docs/semantic-search.md")
	}

	modelPath := filepath.Join(modelDir, "model.onnx")
	vocabPath := filepath.Join(modelDir, "vocab.txt")

	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("model.onnx not found in %s", modelDir)
	}

	vocab, err := loadVocab(vocabPath)
	if err != nil {
		return nil, fmt.Errorf("load vocab: %w", err)
	}

	libPath := findONNXRuntimeLib()
	if libPath == "" {
		return nil, fmt.Errorf("libonnxruntime not found; install via: brew install onnxruntime")
	}
	if err := initializeONNXEnvironment(libPath, os.Getenv, os.Setenv, ort.SetSharedLibraryPath,
		func() error { return ort.InitializeEnvironment() }, ort.DisableTelemetry, ort.DestroyEnvironment); err != nil {
		return nil, err
	}
	p := &ONNXProvider{vocab: vocab}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()

	// Pre-allocate fixed-size tensors.
	shape := ort.Shape{1, onnxMaxSeqLen}
	outputShape := ort.Shape{1, onnxMaxSeqLen, onnxDims}

	p.inputIDs, err = ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("input_ids tensor: %w", err)
	}
	p.attentionMask, err = ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("attention_mask tensor: %w", err)
	}
	p.tokenTypeIDs, err = ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("token_type_ids tensor: %w", err)
	}
	p.output, err = ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("output tensor: %w", err)
	}

	// Create session once with fixed shapes.
	p.session, err = ort.NewAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		[]ort.ArbitraryTensor{p.inputIDs, p.attentionMask, p.tokenTypeIDs},
		[]ort.ArbitraryTensor{p.output},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("ONNX session: %w", err)
	}

	return p, nil
}

func (p *ONNXProvider) Embed(_ context.Context, text string) ([]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.embedLocked(text)
}

func (p *ONNXProvider) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	results := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := p.embedLocked(text)
		if err != nil {
			return nil, fmt.Errorf("embed text %d: %w", i, err)
		}
		results[i] = vec
	}
	return results, nil
}

func (p *ONNXProvider) Dimensions() int { return onnxDims }

func (p *ONNXProvider) Close() error {
	if p.session != nil {
		_ = p.session.Destroy()
	}
	if p.inputIDs != nil {
		_ = p.inputIDs.Destroy()
	}
	if p.attentionMask != nil {
		_ = p.attentionMask.Destroy()
	}
	if p.tokenTypeIDs != nil {
		_ = p.tokenTypeIDs.Destroy()
	}
	if p.output != nil {
		_ = p.output.Destroy()
	}
	return ort.DestroyEnvironment()
}

func (p *ONNXProvider) embedLocked(text string) ([]float32, error) {
	// Tokenize and pad to fixed length.
	tokenIDs := p.tokenize(text)

	// Fill pre-allocated input tensors.
	inputData := p.inputIDs.GetData()
	attData := p.attentionMask.GetData()
	ttData := p.tokenTypeIDs.GetData()

	realTokens := 0
	for i := 0; i < onnxMaxSeqLen; i++ {
		if i < len(tokenIDs) {
			inputData[i] = tokenIDs[i]
			attData[i] = 1
			realTokens++
		} else {
			inputData[i] = padTokenID
			attData[i] = 0
		}
		ttData[i] = 0
	}

	// Run inference (session reused).
	if err := p.session.Run(); err != nil {
		return nil, fmt.Errorf("inference: %w", err)
	}

	// Mean pooling over non-padding tokens.
	outputData := p.output.GetData()
	embedding := make([]float32, onnxDims)
	for i := 0; i < realTokens; i++ {
		for j := 0; j < onnxDims; j++ {
			embedding[j] += outputData[i*onnxDims+j]
		}
	}
	if realTokens > 0 {
		for j := range embedding {
			embedding[j] /= float32(realTokens)
		}
	}

	// L2 normalize.
	var norm float64
	for _, v := range embedding {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm > 1e-10 {
		for j := range embedding {
			embedding[j] /= float32(norm)
		}
	}

	return embedding, nil
}

// tokenize performs basic WordPiece tokenization, padded to onnxMaxSeqLen.
func (p *ONNXProvider) tokenize(text string) []int64 {
	text = strings.ToLower(text)

	var words []string
	var current strings.Builder
	for _, r := range text {
		if r == ' ' || r == '\t' || r == '\n' || r == '/' || r == '.' || r == ':' || r == '_' || r == '-' {
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}

	ids := []int64{clsTokenID}
	for _, word := range words {
		if len(ids) >= onnxMaxSeqLen-1 {
			break
		}
		wordIDs := p.wordPieceTokenize(word)
		for _, id := range wordIDs {
			if len(ids) >= onnxMaxSeqLen-1 {
				break
			}
			ids = append(ids, id)
		}
	}
	ids = append(ids, sepTokenID)
	return ids
}

func (p *ONNXProvider) wordPieceTokenize(word string) []int64 {
	if id, ok := p.vocab[word]; ok {
		return []int64{id}
	}

	var ids []int64
	remaining := word
	for len(remaining) > 0 {
		prefix := remaining
		found := false
		for len(prefix) > 0 {
			lookup := prefix
			if len(ids) > 0 {
				lookup = "##" + prefix
			}
			if id, ok := p.vocab[lookup]; ok {
				ids = append(ids, id)
				remaining = remaining[len(prefix):]
				found = true
				break
			}
			prefix = prefix[:len(prefix)-1]
		}
		if !found {
			ids = append(ids, unkTokenID)
			break
		}
	}
	return ids
}

// --- helpers ---

func findONNXModelDir() string {
	candidates := []string{
		filepath.Join(platform.ModelsDir(), "gte-small"),
		"/tmp/gte-small",
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err == nil {
			return dir
		}
	}
	return ""
}

func findONNXRuntimeLib() string {
	switch runtime.GOOS {
	case "darwin":
		for _, p := range []string{
			"/opt/homebrew/lib/libonnxruntime.dylib",
			"/usr/local/lib/libonnxruntime.dylib",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	case "linux":
		for _, p := range []string{
			"/usr/lib/libonnxruntime.so",
			"/usr/local/lib/libonnxruntime.so",
			"/usr/lib/x86_64-linux-gnu/libonnxruntime.so",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func loadVocab(path string) (map[string]int64, error) {
	if strings.HasSuffix(path, ".gz") {
		return loadVocabGz(path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vocab := make(map[string]int64, 32000)
	scanner := bufio.NewScanner(f)
	var id int64
	for scanner.Scan() {
		line := scanner.Text()
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			word := parts[0]
			fmt.Sscanf(parts[1], "%d", &id)
			vocab[word] = id
		} else {
			vocab[line] = id
			id++
		}
	}
	return vocab, scanner.Err()
}

func loadVocabGz(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	vocab := make(map[string]int64, 32000)
	scanner := bufio.NewScanner(gz)
	var id int64
	for scanner.Scan() {
		line := scanner.Text()
		if parts := strings.SplitN(line, "\t", 2); len(parts) == 2 {
			word := parts[0]
			fmt.Sscanf(parts[1], "%d", &id)
			vocab[word] = id
		} else {
			vocab[line] = id
			id++
		}
	}
	return vocab, scanner.Err()
}
