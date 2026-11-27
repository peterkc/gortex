package resolver

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// FreezeRebuildGDScriptReceiverGate uses the native cold marker before the
// registry runs. Pass the requested selection, not the filtered selection:
// only a request for no synthesizers suppresses the native tail.
func FreezeRebuildGDScriptReceiverGate(ctx context.Context, view store_pebble.RebuildView, requested FrameworkSynthesizerSelection) (bool, error) {
	if requested.none() {
		return false, nil
	}
	var active bool
	if err := view.VisitFrameworkGateNodes(ctx, func(n *graph.Node) error {
		active = active || strings.ToLower(strings.TrimSpace(n.Language)) == "gdscript"
		return nil
	}); err != nil {
		return false, fmt.Errorf("%w: %w", store_pebble.ErrRebuildGDScriptReceiverGate, err)
	}
	return active, nil
}

// RunRebuildGDScriptReceiverGate demotes Calls and References on the settled
// private graph. It does not project relations or filter the caller language.
func RunRebuildGDScriptReceiverGate(ctx context.Context, b *store_pebble.BuildStore, active bool) (uint64, error) {
	if !active {
		return 0, nil
	}
	return b.RebuildGDScriptReceiverGate(ctx, prepareRebuildGDScriptReceiverGate)
}

const rebuildGDScriptGateEvidenceBytes = 16 << 20

type rebuildGDScriptGateEvidence map[graph.NodeKind][]*graph.Node

func (p rebuildGDScriptGateEvidence) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return slices.Values(p[kind])
}

// Keep only the native index fields, in ascending ID order (the cold SQLite
// node-kind index tiebreaker). Charge entry overhead as well as string bytes.
func prepareRebuildGDScriptReceiverGate(nodes iter.Seq2[*graph.Node, error]) (store_pebble.RebuildGDScriptReceiverDemoter, error) {
	evidence := make(rebuildGDScriptGateEvidence, 3)
	budget := rebuildGDScriptGateEvidenceBytes
	for n, err := range nodes {
		if err != nil {
			return nil, err
		}
		p := graph.Node{ID: n.ID, Name: n.Name, FilePath: n.FilePath, Kind: n.Kind, Language: n.Language}
		switch n.Kind {
		case graph.KindType:
			kind, _ := n.Meta["gd_kind"].(string)
			if n.Language == "gdscript" && (kind == "class_name" || kind == "inner_class") {
				parent, _ := n.Meta["gd_extends"].(string)
				opaque, _ := n.Meta["gd_extends_opaque"].(bool)
				p.Meta = map[string]any{"gd_kind": kind, "gd_extends": parent, "gd_extends_opaque": opaque}
			} else if _, script := gdAutoloadDecl(n); script != "" {
				p.Meta = map[string]any{"gd_kind": kind, "gd_script": script}
			} else {
				continue
			}
		case graph.KindVariable:
			if _, script := gdPreloadAliasDecl(n); script == "" {
				continue
			}
			p.Meta = map[string]any{"gd_preload": n.Meta["gd_preload"]}
		}
		budget -= 1024 + len(p.ID) + len(p.Name) + len(p.FilePath)
		for _, v := range p.Meta {
			s, _ := v.(string)
			budget -= len(s)
		}
		if budget < 0 {
			return nil, fmt.Errorf("%w: GDScript receiver evidence", store_pebble.ErrRebuildLinkInputSize)
		}
		evidence[n.Kind] = append(evidence[n.Kind], &p)
	}
	classes := gdClassIndex(evidence)
	if len(classes) == 0 {
		return nil, nil
	}
	autoloads, aliases := gdAutoloadScripts(evidence), gdPreloadAliases(evidence)
	return func(e *graph.Edge, lookup func(string) (*graph.Node, error)) (bool, error) {
		g := rebuildGDScriptGateNodes{lookup: lookup}
		mismatch := gdReceiverMismatch(&g, e, classes, autoloads, aliases)
		if g.err != nil {
			return false, g.err
		}
		if mismatch {
			gdDemoteReceiverMismatch(e)
		}
		return mismatch, nil
	}, nil
}

// Latch any exact node read failure so it cannot turn into a native absence
// verdict (notably for the local declaration check).
type rebuildGDScriptGateNodes struct {
	lookup func(string) (*graph.Node, error)
	err    error
}

func (g *rebuildGDScriptGateNodes) GetNode(id string) (n *graph.Node) {
	if g.err == nil {
		n, g.err = g.lookup(id)
	}
	return n
}
