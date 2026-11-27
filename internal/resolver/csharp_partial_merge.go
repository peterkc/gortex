package resolver

import (
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// mergeCSharpPartialTypes folds the per-file fragments of a C# `partial`
// type into one canonical node. The C# extractor gives every fragment a
// file-scoped ID (filePath + "::" + name), so a `partial class Foo` split
// across Foo.cs / Foo.Designer.cs becomes N type nodes, each carrying a
// fraction of the members with the base list (implements / extends)
// attached only to the fragment that declared it. Left unmerged, interface
// satisfaction is under-detected and find_implementations / get_callers
// return partial results — worst on WebForms designer partials and
// EF / source-generated partials.
//
// The merge key is (repo, Meta["scope_ns"], name) — never bare name, or
// two same-named types in different namespaces (or different workspace
// repositories) would wrongly fuse. (The extractor stamps scope_ns;
// rebindGoMethodReceivers gets away with (dir, name) because a Go
// package == its directory, but a C# namespace ≠ its directory.)
//
// Per group with >1 fragment the lowest ID is canonical (deterministic,
// order-independent); the rest fold their in-edges and out-edges onto it
// (mechanics at the rewrite sites below). The emptied fragment stays —
// the store has no node removal — stamped Meta[metaPartialMergedInto],
// persisted so the DI synthesizer's husk skip holds across restarts.
//
// Scope: csharp class / struct / record only (KindType), mirroring the Go-only
// gate on rebindGoMethodReceivers. Partial INTERFACES are intentionally out of
// scope: InferImplements derives an interface's method set from Meta["methods"]
// (per-fragment, not unioned here), so merging interface fragments would
// under-report interface satisfaction — a separate follow-up.
// Idempotent: a second run re-picks the same canonical and finds the
// fragments' edges already moved, so every rewrite is a no-op.
func (r *Resolver) mergeCSharpPartialTypes() {
	// Same gate as the Go/Python attribution passes: skip the KindType scan
	// outright when the graph indexes no C# at all (the common case in a
	// polyglot workspace with none).
	if !r.graphHasLanguage("csharp") {
		return
	}
	groups := make(map[csharpTypeKey][]*graph.Node)
	for n := range r.nodesByKindLang(graph.KindType, "csharp") {
		if !csharpPartialType(n) {
			continue
		}
		k := csharpTypeKeyOf(n)
		groups[k] = append(groups[k], n)
	}
	r.mergeCSharpPartialGroups(groups)
}

// mergeCSharpPartialTypesForFile is the single-file scope of
// mergeCSharpPartialTypes. Only groups keyed by a type the edited file
// (re-)defines can have gained a fresh unmerged fragment, so the sweep
// collects just those keys' fragments instead of grouping every C# type
// in the graph on each save. A file with no C# types (the common case in
// a polyglot workspace) exits before touching the type index at all.
func (r *Resolver) mergeCSharpPartialTypesForFile(filePath string) {
	var keys map[csharpTypeKey]bool
	for _, n := range r.incrementalFileNodes(filePath) {
		if !csharpPartialType(n) {
			continue
		}
		if keys == nil {
			keys = make(map[csharpTypeKey]bool)
		}
		keys[csharpTypeKeyOf(n)] = true
	}
	if len(keys) == 0 {
		return
	}
	groups := make(map[csharpTypeKey][]*graph.Node)
	for n := range r.nodesByKindLang(graph.KindType, "csharp") {
		if !csharpPartialType(n) {
			continue
		}
		k := csharpTypeKeyOf(n)
		if keys[k] {
			groups[k] = append(groups[k], n)
		}
	}
	r.mergeCSharpPartialGroups(groups)
}

// metaPartialMergedInto marks a merged-away fragment husk with its
// canonical node's ID. The DI synthesizer's husk skip reads it, so the
// marker is persisted, not an in-memory hint.
const metaPartialMergedInto = "partial_merged_into"

// csharpTypeKey identifies one C# type across its partial fragments:
// same workspace repository, same enclosing namespace, same name.
type csharpTypeKey struct{ repo, ns, name string }

func csharpTypeKeyOf(n *graph.Node) csharpTypeKey {
	ns, _ := n.Meta["scope_ns"].(string)
	return csharpTypeKey{n.RepoPrefix, ns, n.Name}
}

func (r *Resolver) mergeCSharpPartialGroups(groups map[csharpTypeKey][]*graph.Node) {
	t0 := time.Now()
	merged := 0
	var batch []graph.EdgeReindex
	var changedNodes []*graph.Node
	for _, frags := range groups {
		if len(frags) < 2 {
			continue
		}
		// Canonical = lowest ID (every group member is KindType by
		// construction of the scan above; class/struct/record are Meta
		// distinctions, not separate kinds).
		canonical := frags[0]
		for _, n := range frags[1:] {
			if n.ID < canonical.ID {
				canonical = n
			}
		}

		fragIDs := make([]string, 0, len(frags)-1)
		for _, n := range frags {
			if n.ID != canonical.ID {
				fragIDs = append(fragIDs, n.ID)
			}
		}
		if len(fragIDs) == 0 {
			continue
		}

		// canonical's existing out-edges — the dedup set for the From-rewrite
		// (and the guard that makes a second run a no-op).
		out := make(map[[2]string]bool)
		for _, e := range r.graph.GetOutEdges(canonical.ID) {
			out[[2]string{string(e.Kind), e.To}] = true
		}

		inByFrag := r.graph.GetInEdgesByNodeIDs(fragIDs)
		outByFrag := r.graph.GetOutEdgesByNodeIDs(fragIDs)

		for _, fid := range fragIDs {
			// To-rewrite: anything pointing AT the fragment now points at
			// canonical (member_of, defines, incoming references).
			for _, e := range inByFrag[fid] {
				if move, ok := csharpPartialRetarget(e, fid, canonical.ID); ok {
					batch = append(batch, move)
				}
			}
			// From-rewrite: edges originating AT the fragment move to
			// canonical (implements / extends), deduped.
			for _, e := range outByFrag[fid] {
				if e.From != fid {
					continue
				}
				sig := [2]string{string(e.Kind), e.To}
				if !out[sig] {
					r.graph.AddEdge(csharpPartialCopyFrom(e, canonical.ID))
					out[sig] = true
				}
				r.graph.RemoveEdge(e.From, e.To, e.Kind)
			}
			if frag := r.graph.GetNode(fid); frag != nil && csharpPartialStamp(frag, metaPartialMergedInto, canonical.ID) {
				changedNodes = append(changedNodes, frag)
			}
		}
		if csharpPartialStamp(canonical, "partial", true) {
			changedNodes = append(changedNodes, canonical)
		}
		merged++
	}
	// Fetched nodes are decoded copies on the SQLite backend: the marker
	// writes above only land via an explicit upsert.
	if len(changedNodes) > 0 {
		r.graph.AddBatch(changedNodes, nil)
	}
	// Batched like the sibling attribution passes: buffered while the
	// incremental cache is active, applied directly otherwise.
	r.persistAttributionReindexes(batch)
	if merged > 0 {
		r.logger.Info("resolver: merged csharp partial types",
			zap.Int("types", merged),
			zap.Int("retargeted_in_edges", len(batch)),
			zap.Duration("took", time.Since(t0)))
	}
}

func csharpPartialType(n *graph.Node) bool {
	return n != nil && n.Kind == graph.KindType && n.Language == "csharp" && n.Name != "" && n.FilePath != ""
}

func csharpPartialRetarget(e *graph.Edge, fragment, canonical string) (graph.EdgeReindex, bool) {
	if e.To != fragment || e.To == canonical {
		return graph.EdgeReindex{}, false
	}
	oldTo := e.To
	e.To = canonical
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}, true
}

func csharpPartialCopyFrom(e *graph.Edge, canonical string) *graph.Edge {
	ne := *e // Preserve the source row's provenance and metadata.
	ne.From = canonical
	return &ne
}

func csharpPartialStamp(n *graph.Node, key string, value any) bool {
	if n.Meta[key] == value {
		return false
	}
	if n.Meta == nil {
		n.Meta = map[string]any{}
	}
	n.Meta[key] = value
	return true
}
