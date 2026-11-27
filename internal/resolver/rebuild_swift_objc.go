package resolver

import (
	"context"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// RunRebuildSwiftObjC executes registry position four after Event channel.
// These private bridges can influence later retained edges, even though
// neither endpoint is in the supported D72 language projection.
func RunRebuildSwiftObjC(ctx context.Context, b *store_pebble.BuildStore, receipts iter.Seq2[store_pebble.ParseFacts, error],
	selection FrameworkSynthesizerSelection, spillAt int,
) (store_pebble.RebuildSwiftObjCStats, error) {
	if !selection.allows(SynthSwiftObjC) {
		return store_pebble.RebuildSwiftObjCStats{}, nil
	}
	return b.RebuildSwiftObjC(ctx, receipts, spillAt, func(view store_pebble.RebuildView, propose func(*graph.Edge) error) error {
		return reduceRebuildSwiftObjC(ctx, view, propose)
	})
}

func reduceRebuildSwiftObjC(ctx context.Context, view store_pebble.RebuildView, propose func(*graph.Edge) error) error {
	var namedSwift, objcSelector, swiftProtocol, objcConformer bool
	var unnamedProtocol *graph.Node
	if err := view.VisitNodes(ctx, func(n *graph.Node) error {
		if !rebuildSwiftObjCNodeKind(n.Kind) {
			return nil
		}
		switch n.Language {
		case "swift":
			namedSwift = namedSwift || n.Name != ""
			if rebuildSwiftObjCProtocol(n) {
				swiftProtocol = true
				if n.Name == "" {
					unnamedProtocol = n // Greatest ID, for SQLite parity.
				}
			}
		case "objc":
			objcSelector = objcSelector || n.Kind == graph.KindMethod && n.Name != ""
			protocols, _ := n.Meta["objc_protocols"].(string)
			objcConformer = objcConformer || n.Kind == graph.KindType && protocols != ""
		}
		return nil
	}); err != nil {
		return err
	}
	if !(namedSwift && objcSelector) && !(swiftProtocol && objcConformer) {
		return nil
	}
	link := func(swift, objc *graph.Node, selector string) error {
		if err := propose(swiftObjCBridgeEdge(swift, objc, selector)); err != nil {
			return err
		}
		return propose(swiftObjCBridgeEdge(objc, swift, selector))
	}
	return view.VisitNodes(ctx, func(n *graph.Node) error {
		if !rebuildSwiftObjCNodeKind(n.Kind) {
			return nil
		}
		switch n.Language {
		case "swift":
			for _, field := range [...]string{"objc_selector", "objc_setter_selector"} {
				sel, _ := n.Meta[field].(string)
				if sel == "" {
					continue
				}
				if err := view.VisitNameCandidates(ctx, sel, []string{"objc"}, func(m *graph.Node) error {
					if m.Kind != graph.KindMethod {
						return nil
					}
					return link(n, m, sel)
				}); err != nil {
					return err
				}
			}
		case "objc":
			if n.Kind == graph.KindMethod && n.Name != "" {
				for _, candidate := range swiftObjCBaseNameCandidates(n.Name) {
					if err := view.VisitNameCandidates(ctx, candidate, []string{"swift"}, func(m *graph.Node) error {
						if !rebuildSwiftObjCNodeKind(m.Kind) {
							return nil
						}
						return link(m, n, n.Name)
					}); err != nil {
						return err
					}
				}
			}
			if protocols, _ := n.Meta["objc_protocols"].(string); n.Kind == graph.KindType && protocols != "" {
				for name := range strings.SplitSeq(protocols, ",") {
					winner, err := rebuildSwiftObjCProtocolWinner(ctx, view, strings.TrimSpace(name), unnamedProtocol)
					if err != nil {
						return err
					}
					if winner != nil && !graph.StructuralEdgeTargetInvalid(graph.EdgeImplements, winner.ID) {
						if err := propose(swiftObjCImplementsEdge(n, winner)); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
}

func rebuildSwiftObjCNodeKind(kind graph.NodeKind) bool {
	switch kind {
	case graph.KindMethod, graph.KindFunction, graph.KindField, graph.KindInterface, graph.KindType:
		return true
	default:
		return false
	}
}

func rebuildSwiftObjCProtocol(n *graph.Node) bool {
	objc, _ := n.Meta["objc"].(bool)
	return n.Kind == graph.KindInterface && objc
}

func rebuildSwiftObjCProtocolWinner(ctx context.Context, view store_pebble.RebuildView, name string, unnamed *graph.Node) (*graph.Node, error) {
	if name == "" {
		return unnamed, nil
	}
	var winner *graph.Node
	// The private index orders repo before ID. For SQLite parity, keep
	// the greatest qualifying ID: native assigns the map slot in ID order.
	err := view.VisitNameCandidates(ctx, name, []string{"swift"}, func(n *graph.Node) error {
		if rebuildSwiftObjCProtocol(n) && (winner == nil || winner.ID < n.ID) {
			winner = n
		}
		return nil
	})
	return winner, err
}
