package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// RebuildTestFiles stamps the file half of cold test projection on private
// scratch. Test symbols and links are separate; this pass never adds edges.
// A skip must be reconstructed from the checked file-stage classification.
func (b *BuildStore) RebuildTestFiles(ctx context.Context, receipts iter.Seq2[ParseFacts, error], skips iter.Seq[*graph.Node],
	isTestFile func(string) bool, defaultRunner func(*graph.Node) string, importRunner func(*graph.Node, *graph.Edge) string,
) (err error) {
	const rowLimit = 16 << 20
	const supported, deferred, unproved, skipped = byte(1), byte(2), byte(4), byte(8)
	const completeDeferred = byte(16)
	prefix := []byte{'g', 'T'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("test file scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	column := func(c byte) []byte { return []byte{'g', 'T', c} }
	key := func(c byte, s ...string) []byte { return keyStrings(column(c), s...) }
	put := func(k, v []byte) error {
		if len(k)+len(v) > rowLimit {
			return fmt.Errorf("%w: test file scratch row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(k, v, pebble.NoSync)
	}
	mark := func(k []byte, bit byte) error {
		prev, err := pendingGet(b.pending, k, copyValue)
		if err != nil {
			return err
		}
		if len(prev) == 1 {
			// The checked census has one receipt per file. A second candidate
			// cannot establish that owner even when its node bytes agree.
			if bit == completeDeferred && prev[0]&completeDeferred != 0 {
				bit = unproved
			}
			if prev[0]&bit != 0 {
				return nil
			}
			bit |= prev[0]
		}
		return put(k, []byte{bit})
	}
	fileProof := func(n *graph.Node, bit byte) error {
		if bit != unproved {
			want, err := encodeNode(projectRebuildReceiptNode(n))
			if err != nil {
				return err
			}
			match, err := pendingGet(b.pending, pendingNodeKey(n.ID), func(raw []byte) (bool, error) {
				return bytes.Equal(raw, want), nil
			})
			if err != nil {
				return err
			}
			if !match {
				bit = unproved
			}
		}
		return mark(key('n', n.ID), bit)
	}
	// Main can change only To for an import from this file. The source and
	// recording site stay fixed; competing receipt classes poison that site.
	importSite := func(e *graph.Edge) []byte { return appendKeyLine(key('i', e.From, e.FilePath), e.Line) }
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		producer := deferred
		switch facts.EffectiveLanguage {
		case "go", "javascript", "typescript":
			producer = supported
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.Kind != graph.KindFile || !isTestFile(n.ID) && !isTestFile(n.FilePath) {
				continue
			}
			bit := producer
			switch {
			case n.ID != file || n.FilePath != file:
				bit = unproved
			case producer == deferred && facts.EffectiveLanguage != "" && facts.File.Errors == "":
				bit = completeDeferred
			}
			if err := fileProof(n, bit); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeImports || !isTestFile(e.From) {
				continue
			}
			bit := producer
			if e.From != file || e.FilePath != file {
				bit = unproved
			}
			if err := mark(importSite(e), bit); err != nil {
				return err
			}
		}
	}
	for n := range skips {
		if err := ctx.Err(); err != nil {
			return err
		}
		if isTestFile(n.FilePath) {
			if err := fileProof(n, skipped); err != nil {
				return err
			}
		}
	}

	view := b.RebuildView()
	var unknown RebuildUnknownProvenanceError
	recordUnknown := func(n *graph.Node) {
		if unknown.Count == 0 {
			unknown.First = graph.EdgeIdentity{From: n.ID, FilePath: n.FilePath}
		}
		unknown.Count++
	}
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := decodeNodeLight(r.value)
		if err != nil || light.Kind != graph.KindFile || !isTestFile(light.FilePath) {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(light.ID)) {
			return invalid("working test file identity does not match key")
		}
		// Native stamps test paths after the workspace writer runs. The
		// exact replacement proves the file without the held raw receipt.
		owned, err := b.rebuildManifestSupersession(light.ID)
		if err != nil {
			return err
		}
		var deferredFile bool
		if !owned {
			proof, err := pendingGet(b.pending, key('n', light.ID), copyValue)
			if err != nil || bytes.Equal(proof, []byte{deferred}) {
				return err
			}
			deferredFile = bytes.Equal(proof, []byte{completeDeferred})
			if !deferredFile && !bytes.Equal(proof, []byte{supported}) && !bytes.Equal(proof, []byte{skipped}) {
				recordUnknown(light)
				return nil
			}
		}
		if deferredFile {
			// Native runner detection can read outgoing imports before using its
			// default. A missing working edge cannot disprove a raw import site.
			for _, prefix := range [][]byte{key('i', light.ID), guardOutgoing(light.ID, graph.EdgeImports)} {
				found, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
				if err != nil || found {
					return err
				}
			}
		}
		if len(r.key)+len(r.value) > rowLimit {
			return fmt.Errorf("%w: test file row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		runner := defaultRunner(n)
		if stamp, _ := n.Meta["test_runner"].(string); stamp == "" && !deferredFile {
			// Native takes the first recognized import. Lock it to the
			// complete working key, not parser line or node arrival order.
			var firstUnproved, proved string
			var conflictingUnproved bool
			if err := view.visit(ctx, guardOutgoing(n.ID, graph.EdgeImports), func(_ *pebble.Snapshot, r row) error {
				if proved != "" {
					return nil
				}
				if len(r.key)+len(r.value) > rowLimit {
					return fmt.Errorf("%w: test runner import", ErrRebuildLinkInputSize)
				}
				e, err := decodeEdge(r.value)
				if err != nil {
					return err
				}
				if !bytes.Equal(r.key, pendingEdgeKey(e)) {
					return invalid("test runner import identity does not match key")
				}
				candidate := importRunner(n, e)
				if candidate == "" {
					return nil
				}
				p, err := pendingGet(b.pending, importSite(e), copyValue)
				if err != nil {
					return err
				}
				switch {
				case bytes.Equal(p, []byte{supported}):
					proved = candidate
				case firstUnproved == "":
					firstUnproved = candidate
				case candidate != firstUnproved:
					conflictingUnproved = true
				}
				return nil
			}); err != nil {
				return err
			}
			if proved == "" {
				proved = runner
			}
			// Any subset of unproved rows before the proved winner could be
			// absent after suppression. Each possible winner must agree.
			if conflictingUnproved || firstUnproved != "" && firstUnproved != proved {
				recordUnknown(n)
				return nil
			}
			runner = proved
		}
		flag, _ := n.Meta["is_test_file"].(bool)
		if prior, _ := n.Meta["test_runner"].(string); flag && (runner == "" || prior == runner) {
			return nil
		}
		if n.Meta == nil {
			n.Meta = make(map[string]any, 2)
		}
		n.Meta["is_test_file"] = true
		if runner != "" {
			n.Meta["test_runner"] = runner
		}
		value, err := encodeNode(n)
		if err != nil {
			return err
		}
		return put(key('w', n.ID), value)
	}); err != nil {
		return err
	}
	if unknown.Count != 0 {
		return &unknown
	}
	return b.withWrites(func() error {
		return pendingVisit(ctx, b.pending, column('w'), func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			owned, err := b.rebuildManifestSupersession(n.ID)
			if err != nil {
				return err
			}
			if err := b.writeNode(n); err != nil {
				return err
			}
			if !owned {
				return nil
			}
			// Restore must verify the stamped row, not the writer's
			// earlier bytes or arbitrary later changes. Save the bytes
			// writeNode stored, as AddRebuildManifestArtifacts does.
			nodeKey := pendingNodeKey(n.ID)
			raw, err := pendingGet(b.pending, nodeKey, copyValue)
			if err != nil {
				return err
			}
			if raw == nil {
				return invalid("stamped manifest replacement was not saved")
			}
			return b.pending.Set(rebuildHoldoutKey('s', nodeKey), raw, pebble.NoSync)
		})
	})
}
