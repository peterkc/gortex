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

var ErrRebuildSvelteKitLoad = errors.New("pebble rebuild SvelteKit load references failed")

var rebuildSvelteKitLoadPrefix = []byte{'g', 'q', 'k'}

// RebuildSvelteKitLoadRules supplies the native route roles and Reference.
type RebuildSvelteKitLoadRules struct {
	RouteNode func(graph.NodeKind, *graph.Node) (string, bool)
	ServerDir func(string) (string, bool)
	PageFile  func(string) bool
	Target    func(*graph.Node, string, *graph.Node, graph.FileNodeIdentity) string
	Reference func(*graph.Node, string) *graph.Edge
}

type rebuildSvelteKitLoad struct {
	rebuildCallMoves
	rules RebuildSvelteKitLoadRules
}

// RebuildSvelteKitLoad installs only missing References with proved route elections.
func (b *BuildStore) RebuildSvelteKitLoad(ctx context.Context, receipts iter.Seq2[ParseFacts, error], skips iter.Seq[*graph.Node], rules RebuildSvelteKitLoadRules) (adds uint64, err error) {
	defer func() {
		if err != nil {
			adds = 0
			err = fmt.Errorf("%w: %w", ErrRebuildSvelteKitLoad, err)
		}
	}()
	if receipts == nil || rules.RouteNode == nil || rules.ServerDir == nil || rules.PageFile == nil || rules.Target == nil || rules.Reference == nil {
		return 0, invalid("SvelteKit load inputs")
	}
	prefix := rebuildSvelteKitLoadPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("SvelteKit load scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildSvelteKitLoad{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "SvelteKit load", inputRow: "node row"},
		rules:            rules,
	}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	if err := p.record(ctx, receipts, skips); err != nil {
		return 0, err
	}
	return p.planAndWrite(ctx)
}

func (p *rebuildSvelteKitLoad) role(n *graph.Node) (dir string, ok bool) {
	if n.Kind == graph.KindFile {
		return p.rules.ServerDir(n.FilePath)
	}
	return p.rules.RouteNode(n.Kind, n)
}

func (p *rebuildSvelteKitLoad) winner(col byte, dir string, kind graph.NodeKind) (*graph.Node, error) {
	return pendingGet(p.b.pending, p.key(col, dir, string(kind)), decodeNodeLight)
}

func (p *rebuildSvelteKitLoad) offer(col byte, dir string, n *graph.Node) error {
	prior, err := p.winner(col, dir, n.Kind)
	if err != nil || prior != nil && prior.ID >= n.ID {
		return err
	}
	candidate := graph.Node{ID: n.ID, Kind: n.Kind, FilePath: n.FilePath, WorkspaceID: n.WorkspaceID}
	if n.Kind == graph.KindType {
		candidate.StartLine = n.StartLine
	}
	value, err := encodeNode(&candidate)
	if err != nil {
		return err
	}
	if err := p.put(p.key(col, dir, string(n.Kind)), value); err != nil {
		return err
	}
	return p.put(p.key('d', dir), nil)
}

func (p *rebuildSvelteKitLoad) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		switch n.Kind {
		case graph.KindFile, graph.KindFunction:
		case graph.KindType:
			if !p.rules.PageFile(n.FilePath) {
				return nil
			}
			n, err = decodeNode(r.value)
			if err != nil {
				return err
			}
		default:
			return nil
		}
		dir, ok := p.role(n)
		if !ok {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.offer('l', dir, n)
	})
}

func (p *rebuildSvelteKitLoad) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error], skips iter.Seq[*graph.Node]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n == nil || n.FilePath != file || n.Kind == graph.KindFile && n.ID != file {
				continue
			}
			if err := p.recordNode(n); err != nil {
				return err
			}
		}
	}
	if skips != nil {
		for n := range skips {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n == nil || n.Kind != graph.KindFile || n.ID != n.FilePath {
				return invalid("SvelteKit checked skip row")
			}
			if err := p.recordNode(n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildSvelteKitLoad) recordNode(n *graph.Node) error {
	dir, ok := p.role(n)
	if !ok {
		return nil
	}
	if n.Kind == graph.KindFile {
		// Reuse the test-file stage's exact FileGraph row check. The
		// checked skip stream is a separate producer, not a parse receipt.
		value, err := encodeNode(n)
		if err != nil {
			return err
		}
		matched, err := pendingGet(p.b.pending, pendingNodeKey(n.ID), func(raw []byte) (bool, error) {
			return bytes.Equal(raw, value), nil
		})
		if err != nil {
			return err
		}
		key := p.key('f', n.ID)
		state, err := rebuildScratchState(p.b.pending, key, "SvelteKit server file proof")
		if err != nil {
			return err
		}
		if err := noteRebuildScratchState(p.put, key, state|rebuildCapabilityWanted, rebuildScratchProofBit(matched)); err != nil {
			return err
		}
	}
	return p.offer('r', dir, n)
}

func (p *rebuildSvelteKitLoad) electedProved(dir string) (bool, error) {
	load, err := p.winner('l', dir, graph.KindFunction)
	if err != nil {
		return false, err
	}
	kind := graph.KindFile
	if load != nil && load.ID != "" {
		kind = graph.KindFunction
	} else {
		file, err := p.winner('l', dir, graph.KindFile)
		if err != nil || file == nil {
			return false, err
		}
		state, err := rebuildScratchState(p.b.pending, p.key('f', file.ID), "SvelteKit server file proof")
		if err != nil || state != rebuildScratchExact {
			return false, err
		}
	}
	for _, role := range [...]graph.NodeKind{graph.KindType, kind} {
		live, err := p.get(p.key('l', dir, string(role)))
		if err != nil {
			return false, err
		}
		raw, err := p.get(p.key('r', dir, string(role)))
		if err != nil || !bytes.Equal(live, raw) {
			return false, err
		}
	}
	return true, nil
}

func (p *rebuildSvelteKitLoad) reference(col byte, dir string) (*graph.Edge, error) {
	page, err := p.winner(col, dir, graph.KindType)
	if err != nil || page == nil {
		return nil, err
	}
	load, err := p.winner(col, dir, graph.KindFunction)
	if err != nil {
		return nil, err
	}
	var loadID string
	var file graph.FileNodeIdentity
	if load != nil && load.ID != "" {
		loadID = load.ID
	} else {
		server, err := p.winner(col, dir, graph.KindFile)
		if err != nil {
			return nil, err
		}
		if server != nil {
			file = graph.FileNodeIdentity{ID: server.ID, WorkspaceID: server.WorkspaceID}
		}
	}
	to := p.rules.Target(page, loadID, load, file)
	if to == "" {
		return nil, nil
	}
	return p.rules.Reference(page, to), nil
}

func (p *rebuildSvelteKitLoad) fresh(e *graph.Edge) (bool, error) {
	if e == nil {
		return false, nil
	}
	occupied, err := p.present(pendingEdgeKey(e))
	return !occupied, err
}

func (p *rebuildSvelteKitLoad) planAndWrite(ctx context.Context) (uint64, error) {
	var adds uint64
	var refused rebuildScratchRefusals
	prefix := p.column('d')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		dir, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return invalid("SvelteKit route directory")
		}
		live, err := p.reference('l', dir)
		if err != nil {
			return err
		}
		raw, err := p.reference('r', dir)
		if err != nil {
			return err
		}
		liveFresh, err := p.fresh(live)
		if err != nil {
			return err
		}
		rawFresh, err := p.fresh(raw)
		if err != nil || !liveFresh && !rawFresh {
			return err
		}
		if !liveFresh || !rawFresh || graph.EdgeIdentityFor(live) != graph.EdgeIdentityFor(raw) {
			if liveFresh {
				refused.add(graph.EdgeIdentityFor(live), pendingEdgeKey(live))
			}
			if rawFresh {
				refused.add(graph.EdgeIdentityFor(raw), pendingEdgeKey(raw))
			}
			return nil
		}
		proved, err := p.electedProved(dir)
		if err != nil {
			return err
		}
		if !proved {
			refused.add(graph.EdgeIdentityFor(live), pendingEdgeKey(live))
			return nil
		}
		value, err := encodeEdge(live)
		if err != nil {
			return err
		}
		adds++
		return p.put(p.key('w', dir), value)
	}); err != nil {
		return 0, err
	}
	if err := refused.err(); err != nil || adds == 0 {
		return 0, err
	}
	return adds, p.b.withWrites(func() error {
		return pendingVisit(ctx, p.b.pending, p.column('w'), func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofSvelteKitLoad})
		})
	})
}
