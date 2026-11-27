package resolver

import (
	"context"
	"fmt"
	"iter"
	"slices"

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
	active, err := view.HasFrameworkGateNode(ctx, func(n *graph.Node) (bool, error) {
		return normalizeFrameworkLanguage(n.Language) == "gdscript", nil
	})
	if err != nil {
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

// Fold only the native declaration fields, in ascending ID order. The
// class index decides whether script declarations can affect the gate.
func prepareRebuildGDScriptReceiverGate(classRows, scripts iter.Seq2[*graph.Node, error]) (store_pebble.RebuildGDScriptReceiverDemoter, error) {
	classes := make(map[string]*gdClass)
	budget := rebuildGDScriptGateEvidenceBytes
	charge := func(size int) error {
		budget -= 1024 + size // Include index entry overhead.
		if budget < 0 {
			return fmt.Errorf("%w: GDScript receiver evidence", store_pebble.ErrRebuildLinkInputSize)
		}
		return nil
	}
	for n, err := range classRows {
		if err != nil {
			return nil, err
		}
		if class, ok := gdClassDecl(n); ok {
			if err := charge(len(n.Name) + len(class.fileID) + len(class.parent)); err != nil {
				return nil, err
			}
			gdIndexClass(classes, n.Name, class)
		}
	}
	if len(classes) == 0 {
		return nil, nil
	}
	decls := make(map[string]string)
	preloads := make(map[string]map[string]string)
	var files []*graph.Node
	for n, err := range scripts {
		if err != nil {
			return nil, err
		}
		if isGDScriptFile(n) {
			if err := charge(len(n.ID) + len(n.FilePath)); err != nil {
				return nil, err
			}
			files = append(files, &graph.Node{ID: n.ID, Kind: n.Kind, Language: n.Language, FilePath: n.FilePath})
		} else if name, path := gdAutoloadDecl(n); path != "" {
			if err := charge(len(name) + len(path)); err != nil {
				return nil, err
			}
			gdRecordAutoload(decls, name, path)
		} else if fileID, path := gdPreloadAliasDecl(n); path != "" {
			if err := charge(len(fileID) + len(n.Name) + len(path)); err != nil {
				return nil, err
			}
			gdRecordPreloadAlias(preloads, fileID, n.Name, path)
		}
	}
	var autoloads map[string]string
	if len(decls) != 0 {
		autoloads = gdResolveAutoloadScripts(decls, slices.Values(files))
	}
	var aliases map[string]map[string]string
	if len(preloads) != 0 {
		aliases = gdResolvePreloadAliases(preloads, slices.Values(files))
	}
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
