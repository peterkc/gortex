package graph

import "strings"

// Lazy builtin-sentinel materialization.
//
// applyBuiltinIfKnown rewrites unresolvable method calls to
// `[<repo>::]builtin::<lang>::<category>::<method>` stub targets, but only
// the Go attribution pass ever materialized KindBuiltin nodes — a production
// audit found 16k dangling calls to TS/JS builtin stubs (`array::push`,
// `string::split`, …) with no node behind them. Mirroring the externals
// pattern, the write funnels materialize the node the first time an edge
// targets the stub: idempotent upserts, no reads, no resolver involvement.

const builtinStubInfix = "::" + StubKindBuiltin + "::"

// BuiltinStubNodes returns one KindBuiltin node per distinct builtin stub
// target in edges, deduplicated within the batch. Node shape mirrors
// go_builtins_attribution's materialization. Both StubID forms are
// recognised: the repo-prefixed `<repo>::builtin::…` and the solo-repo
// `builtin::…` (an empty repo prefix elides the leading segment entirely).
func BuiltinStubNodes(edges []*Edge) []*Node {
	var out []*Node
	var seen map[string]struct{}
	for _, e := range edges {
		if e == nil || e.To == "" || !IsBuiltinStub(e.To) {
			continue
		}
		repoPrefix := ""
		rest := ""
		if infix := strings.Index(e.To, builtinStubInfix); infix > 0 {
			repoPrefix = e.To[:infix]
			rest = e.To[infix+len(builtinStubInfix):]
		} else if strings.HasPrefix(e.To, StubKindBuiltin+"::") {
			rest = e.To[len(StubKindBuiltin+"::"):]
		} else {
			continue
		}
		if seen == nil {
			seen = make(map[string]struct{}, 4)
		}
		if _, dup := seen[e.To]; dup {
			continue
		}
		seen[e.To] = struct{}{}
		segments := strings.Split(rest, "::")
		if len(segments) < 2 {
			continue
		}
		lang := segments[0]
		name := segments[len(segments)-1]
		if lang == "" || name == "" {
			continue
		}
		// The category segments name the kind (`type`, `const`, `array`);
		// a function ID has none, and go_builtins_attribution stores "func"
		// for it. Both constructors must agree, or the stored row depends on
		// which one runs first.
		kind := strings.Join(segments[1:len(segments)-1], "::")
		if kind == "" {
			kind = "func"
		}
		out = append(out, &Node{
			ID:         e.To,
			Kind:       KindBuiltin,
			Name:       name,
			Language:   lang,
			RepoPrefix: repoPrefix,
			Meta: map[string]any{
				"builtin":      true,
				"builtin_kind": kind,
			},
		})
	}
	return out
}
