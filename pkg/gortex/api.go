// Package gortex provides a public API for embedding the Gortex code intelligence engine.
package gortex

import (
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/storage/backend"
)

// Engine is the public entry point for the Gortex code intelligence engine.
//
// An Engine owns a SQLite graph store — an open database handle plus the
// background bookkeeping that keeps its write-ahead log in check — so every
// Engine must be closed when the caller is done with it. See Close.
type Engine struct {
	store      graph.Store
	closeStore func() error
	indexer    *indexer.Indexer
	query      *query.Engine

	// tmpDir is non-empty when New created the store in a temp directory it
	// owns; Close removes it.
	tmpDir string
}

// Option configures an Engine.
type Option func(*settings)

// settings collects everything the options can influence before the Engine
// and its store are constructed.
type settings struct {
	index     config.IndexConfig
	storePath string
}

// WithWorkers sets the number of parallel parsing workers.
func WithWorkers(n int) Option {
	return func(s *settings) { s.index.Workers = n }
}

// WithExclude adds exclude patterns.
func WithExclude(patterns ...string) Option {
	return func(s *settings) { s.index.Exclude = append(s.index.Exclude, patterns...) }
}

// WithStorePath puts the graph store at path, creating the file and any
// missing parent directories. The store survives Close, so a later Engine
// opened on the same path starts from the graph the previous run indexed.
//
// Without this option New keeps the store in a temp directory that Close
// deletes, which suits one-shot analysis but throws the index away.
func WithStorePath(path string) Option {
	return func(s *settings) { s.storePath = path }
}

// New creates a new Gortex Engine with the given options.
//
// The caller owns the returned Engine's store and must call Close on it.
func New(opts ...Option) (*Engine, error) {
	cfg := config.Default()
	set := &settings{index: cfg.Index}
	for _, o := range opts {
		o(set)
	}

	path := set.storePath
	tmpDir := ""
	if path == "" {
		dir, err := os.MkdirTemp("", "gortex-engine-*")
		if err != nil {
			return nil, fmt.Errorf("create temporary graph store directory: %w", err)
		}
		tmpDir = dir
		path = filepath.Join(dir, "graph.sqlite")
	} else if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create graph store directory %s: %w", dir, err)
		}
	}

	st, closeStore, err := backend.OpenRaw("sqlite", path)
	if err != nil {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
		return nil, fmt.Errorf("open graph store %s: %w", path, err)
	}

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	return &Engine{
		store:      st,
		closeStore: closeStore,
		indexer:    indexer.New(st, reg, set.index, zap.NewNop()),
		query:      query.NewEngine(st),
		tmpDir:     tmpDir,
	}, nil
}

// Close releases the graph store: it checkpoints the write-ahead log and
// closes the database handle, and removes the temp directory when New created
// one. Every Engine must be closed exactly once; calling it a second time is a
// no-op. After Close the Engine's query and index methods must not be used.
func (e *Engine) Close() error {
	if e == nil {
		return nil
	}
	var err error
	if e.store != nil {
		err = e.closeStore()
		e.store = nil
		e.closeStore = nil
	}
	if e.tmpDir != "" {
		if rmErr := os.RemoveAll(e.tmpDir); rmErr != nil && err == nil {
			err = rmErr
		}
		e.tmpDir = ""
	}
	return err
}

// IndexResult is the result of an indexing operation.
type IndexResult = indexer.IndexResult

// Index walks root and populates the knowledge graph.
func (e *Engine) Index(root string) (*IndexResult, error) {
	return e.indexer.Index(root)
}

// IndexFile re-indexes a single file (evict + re-parse).
func (e *Engine) IndexFile(filePath string) error {
	return e.indexer.IndexFile(filePath)
}

// EvictFile removes all data for a file from the graph.
func (e *Engine) EvictFile(filePath string) (int, int) {
	return e.indexer.EvictFile(filePath)
}

// GetSymbol returns a node by ID.
func (e *Engine) GetSymbol(id string) *graph.Node {
	return e.query.GetSymbol(id)
}

// FindSymbols finds nodes matching a name, optionally filtered by kind.
func (e *Engine) FindSymbols(name string, kinds ...graph.NodeKind) []*graph.Node {
	return e.query.FindSymbols(name, kinds...)
}

// SubGraph is a query result.
type SubGraph = query.SubGraph

// GetDependencies returns outgoing dependencies.
func (e *Engine) GetDependencies(nodeID string, depth, limit int) *SubGraph {
	return e.query.GetDependencies(nodeID, query.QueryOptions{Depth: depth, Limit: limit, Detail: "brief"})
}

// GetDependents returns the blast radius for a symbol.
func (e *Engine) GetDependents(nodeID string, depth, limit int) *SubGraph {
	return e.query.GetDependents(nodeID, query.QueryOptions{Depth: depth, Limit: limit, Detail: "brief"})
}

// GetCallChain traces the call graph forward from a function.
func (e *Engine) GetCallChain(funcID string, depth, limit int) *SubGraph {
	return e.query.GetCallChain(funcID, query.QueryOptions{Depth: depth, Limit: limit, Detail: "brief"})
}

// Stats returns summary statistics for the graph.
func (e *Engine) Stats() *graph.GraphStats {
	return e.query.Stats()
}
