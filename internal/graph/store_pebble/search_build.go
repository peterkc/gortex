package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/fts5token"
)

func (b *BuildStore) UpsertSymbolFTS(id, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first != nil {
		return b.first
	}
	n, err := b.pendingNode(id)
	if err != nil {
		return err
	}
	if id == "" || n == nil {
		return invalid("symbol document has no source node")
	}
	err = b.pending.Set(pendingSymbolKey(id), []byte(text), pebble.NoSync)
	if err != nil {
		b.first = err
	}
	return err
}

func (b *BuildStore) BulkUpsertSymbolFTS(repo string, items []graph.SymbolFTSItem) (err error) {
	if len(items) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first != nil {
		return b.first
	}
	for _, item := range items {
		n, err := b.pendingNode(item.NodeID)
		if err != nil {
			return err
		}
		if item.NodeID == "" || n == nil || (repo != "" && n.RepoPrefix != repo) {
			return invalid("symbol document has no source node in repository")
		}
	}
	defer func() {
		if err != nil {
			b.first = err
		}
	}()
	prefix := []byte{familySymbol}
	if err := pendingVisit(context.Background(), b.pending, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[1:])
		if err != nil {
			return err
		}
		n, err := b.pendingNode(id)
		if err != nil {
			return err
		}
		if n != nil && n.RepoPrefix == repo {
			return b.pending.Delete(r.key, pebble.NoSync)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, item := range items {
		if err := b.pending.Set(pendingSymbolKey(item.NodeID), []byte(item.Tokens), pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

// Postings are built by Publish, with the final graph and document set.
func (b *BuildStore) BuildSymbolIndex() error { return nil }

func (b *BuildStore) SearchSymbols(q string, limit int) (_ []graph.SymbolHit, err error) {
	s, err := b.OpenSnapshot()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	return s.SearchSymbols(q, limit)
}

var _ graph.SymbolSearcher = (*BuildStore)(nil)

func symbolPrefixes(term string, visit func(string)) {
	for n := 1; n <= len(term) && term[n-1] < 0x80; n++ {
		visit(term[:n])
	}
}

type searchExactSource uint8

const (
	exactFromNodes searchExactSource = iota
	exactFromDocuments
)

// writeSymbols assigns IDs in canonical logical node order. Prefix frequencies
// spill to the same private index rather than growing with the corpus heap.
func (b *BuildStore) writeSymbols(ctx context.Context, gold *sortedRuns, exactSource searchExactSource) error {
	dfPrefix, idPrefix, docPrefix := []byte{'D'}, []byte{'I'}, []byte{'B'}
	for _, prefix := range [][]byte{dfPrefix, idPrefix, docPrefix} {
		if err := b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
			return err
		}
	}
	var stats searchStats
	repos := make(map[string]struct{})
	writeExact := func(n *graph.Node) error {
		if searchExactKind(string(n.Kind)) && b.profile.keepsNode(n.Kind) {
			return gold.add(searchKey("exact", n.RepoPrefix, n.Name, n.ID), []byte(n.ID))
		}
		return nil
	}
	postings := sortedRuns{dir: gold.dir, name: "posts", options: gold.options, format: gold.format, limit: goldBufferBytes}
	ids, texts := make([]string, 0, 256), make([]string, 0, 256)
	writeDocs := func() error {
		if len(ids) == 0 {
			return nil
		}
		tokens, err := fts5token.SymbolFTSTokenize(ctx, texts)
		if err != nil {
			return err
		}
		for j, terms := range tokens {
			id := ids[j]
			n, err := b.pendingNode(id)
			if err != nil {
				return err
			}
			if n == nil {
				return invalid("symbol document has no source node")
			}
			if !b.profile.keepsNode(n.Kind) {
				continue
			}
			// A selected document can have no terms and still match its exact name.
			if exactSource == exactFromDocuments {
				if err := writeExact(n); err != nil {
					return err
				}
			}
			stats.Rows++
			stats.Count++
			d := searchDoc{Rowid: stats.Rows, Length: int64(len(terms)), NodePresent: true, ID: id, Repo: n.RepoPrefix, Path: searchPath(n.FilePath, n.RepoPrefix), Name: n.Name, Kind: string(n.Kind)}
			freq := make(map[string]int64)
			for _, term := range terms {
				freq[term]++
			}
			seen := make(map[string]struct{})
			for _, term := range slices.Sorted(maps.Keys(freq)) {
				d.Terms = append(d.Terms, searchTerm{term, freq[term]})
				symbolPrefixes(term, func(prefix string) { seen[prefix] = struct{}{} })
			}
			for prefix := range seen {
				if err := addPendingCount(b.pending, keyStrings(bytes.Clone(dfPrefix), prefix), 1); err != nil {
					return err
				}
			}
			repos[d.Repo] = struct{}{}
			stats.Tokens += d.Length
			value := searchPackDoc(d)
			if err := gold.add(binary.BigEndian.AppendUint64(searchKey("doc"), uint64(d.Rowid)), value); err != nil {
				return err
			}
			if len(d.Terms) > 0 {
				key := binary.BigEndian.AppendUint64(bytes.Clone(docPrefix), uint64(d.Rowid))
				if err := b.pending.Set(key, value, pebble.NoSync); err != nil {
					return err
				}
			}
			for _, term := range d.Terms {
				key := binary.BigEndian.AppendUint64(searchKey("post", d.Repo, term.Text, d.Path), uint64(d.Rowid))
				if err := postings.add(key, nil); err != nil {
					return err
				}
			}
		}
		clear(ids)
		clear(texts)
		ids, texts = ids[:0], texts[:0]
		return nil
	}
	if err := pendingVisit(ctx, b.pending, []byte{familySymbol}, func(r row) error {
		id, _, err := readKeyString(r.key[1:])
		if err != nil {
			return err
		}
		ids = append(ids, id)
		texts = append(texts, string(r.value))
		if len(ids) == cap(ids) {
			return writeDocs()
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeDocs(); err != nil {
		return err
	}
	if exactSource == exactFromNodes {
		// Legacy Publish also exposes exact names of nodes with no symbol document.
		if err := pendingVisit(ctx, b.pending, []byte{familyNode}, func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			if !b.profile.keepsNode(n.Kind) {
				return nil
			}
			repos[n.RepoPrefix] = struct{}{}
			return writeExact(n)
		}); err != nil {
			return err
		}
	}
	stats.Repos = slices.Sorted(maps.Keys(repos))
	if err := gold.add(searchKey("stats"), searchPackStats(stats)); err != nil {
		return err
	}
	seq := 0
	if err := pendingVisit(ctx, b.pending, dfPrefix, func(r row) error {
		prefix, _, err := readKeyString(r.key[1:])
		if err != nil {
			return err
		}
		df, err := decodeCount(r.value)
		if err != nil {
			return err
		}
		seq++
		if uint64(seq) > math.MaxUint32 {
			return invalid("too many search prefixes")
		}
		v := binary.AppendVarint(nil, int64(df))
		v = binary.AppendVarint(v, int64(seq))
		if err := gold.add(searchKey("prefix-df", prefix), v); err != nil {
			return err
		}
		return b.pending.Set(keyStrings(bytes.Clone(idPrefix), prefix), encodeCount(seq), pebble.NoSync)
	}); err != nil {
		return err
	}
	avgdl := float64(stats.Tokens) / float64(stats.Rows)
	prefixID := func(prefix string) (uint32, error) {
		id, err := pendingGet(b.pending, keyStrings(bytes.Clone(idPrefix), prefix), decodeCount)
		return uint32(id), err
	}
	if err := pendingVisit(ctx, b.pending, docPrefix, func(r row) error {
		doc, err := searchReadDoc(r.value)
		if err != nil {
			return err
		}
		bounds, err := searchDocBounds(doc, avgdl, prefixID)
		if err != nil {
			return err
		}
		return b.pending.Set(r.key, bounds, pebble.NoSync)
	}); err != nil {
		return fmt.Errorf("prepare symbol bounds: %w", err)
	}
	var block []searchPosting
	maxima := make(map[uint32]float32)
	var group, start, termKey []byte
	flush := func() error {
		if len(block) == 0 {
			return nil
		}
		value := searchPackBlock(block, maxima)
		block = block[:0]
		clear(maxima)
		return gold.add(start, value)
	}
	lead := searchKey("post")
	if err := postings.merge(ctx, func(r row) error {
		if len(r.key) < len(lead)+8 {
			return invalid("posting key width")
		}
		key := r.key[:len(r.key)-8]
		if !bytes.Equal(group, key) || len(block) == 128 {
			if err := flush(); err != nil {
				return err
			}
			group = append(group[:0], key...)
			start = bytes.Clone(r.key)
		}
		repo, t, err := readKeyString(r.key[len(lead):])
		if err != nil {
			return err
		}
		term, _, err := readKeyString(t)
		if err != nil {
			return err
		}
		k := searchKey("term", repo, term)
		if !bytes.Equal(k, termKey) {
			if err := gold.add(k, []byte(term)); err != nil {
				return err
			}
			termKey = k
		}
		id := int64(binary.BigEndian.Uint64(r.key[len(key):]))
		boundKey := binary.BigEndian.AppendUint64(bytes.Clone(docPrefix), uint64(id))
		posting, err := pendingGet(b.pending, boundKey, func(raw []byte) (searchPosting, error) {
			present, err := mergeSearchDocBounds(raw, maxima)
			return searchPosting{Rowid: id, NodePresent: present}, err
		})
		if err != nil {
			return err
		}
		if posting.Rowid <= 0 {
			return invalid("missing symbol document bounds")
		}
		block = append(block, posting)
		return nil
	}); err != nil {
		return fmt.Errorf("build symbol postings: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	return b.pending.DeleteRange(docPrefix, prefixEnd(docPrefix), pebble.NoSync)
}
