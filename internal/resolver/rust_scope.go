package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveRustScopeCalls is the graph-wide materialisation pass for the
// Rust-specific scope layer. It lands Rust call edges the generic
// resolver leaves unresolved by applying Rust's own scoping rules:
//
//  1. impl-block method owner. `Foo::new()` resolves to the `new`
//     method defined in `impl Foo { fn new(...) }`. The Rust extractor
//     stamps the full scoped path on the call edge as Meta["rust_path"]
//     ("Foo::new"); this pass reads the qualifier ("Foo") and binds the
//     trailing segment to a method whose owner type (Node.Meta
//     ["receiver"]) is that qualifier. Resolved at ast_resolved — the
//     receiver type is named in source, so the binding is structurally
//     unambiguous within the qualifier's type.
//
//  2. self / Self receiver. Inside `impl Foo`, `self.bar()` and
//     `Self::new()` resolve to Foo's methods. The caller is a Rust
//     method node carrying Meta["receiver"]="Foo", so the enclosing
//     impl type is read off the caller and the call binds to a method
//     of that owner. Resolved at ast_resolved.
//
//  3. module-path. `crate::module::func()`, `super::func()`,
//     `self::func()` and `module::func()` resolve to a free function
//     named by the path's trailing segment. Gortex does not model the
//     Rust module tree as graph nodes, so the binding matches the
//     trailing segment against free functions in the caller's repo,
//     preferring a same-file then same-directory candidate. Resolved at
//     ast_inferred — the module prefix is not verified against a real
//     module node, only the trailing name and locality.
//
// Local-shadows-import precedence: before binding a module-path call to
// a free function, the pass checks whether the caller has a parameter
// of the same name (a local binding that, in Rust, shadows an imported
// item of the same identifier). When it does, the call is left
// unresolved rather than bound to the (shadowed) import target.
//
// The pass only ever rewrites an edge whose target is still an
// `unresolved::` placeholder, so it never fights or overrides a binding
// the generic resolver already landed; it strictly fills in the
// residual the generic pass missed. It is a full recompute and
// idempotent — each candidate edge's target is recomputed from its own
// Meta on every run, so a reindex of either endpoint's file leaves the
// edge's resolution stable. graph.ReindexEdges keeps the out/in buckets
// consistent.
//
// Ambiguity is resolved conservatively: when more than one candidate
// matches, the pass skips the edge (zero false positives over breadth).
//
// Out of scope (left for the generic resolver, the cross-repo resolver,
// or future work): cross-repo Rust calls, fully-qualified
// `<T as Trait>::method` UFCS, and resolving a module path against a real
// module-tree node (only the trailing segment + locality is used today).
//
// Returns the number of Rust module-import, supertrait, override, and call
// edges this pass landed on concrete nodes.
func ResolveRustScopeCalls(g graph.Store) int {
	return resolveRustScopeCalls(g, nil)
}

// resolveRustScopeCalls is the census-multiplexed form: cands, when
// non-nil, supplies the pre-matched unresolved-call candidates and the
// shared node snapshot for the scope index's method / function / param
// sweeps. The module-import, trait-override, alias, and field scans keep
// their own streams — their kinds are not part of the shared walks.
func resolveRustScopeCalls(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}

	// Module-path `use crate::…`/`self::…`/`super::…` → module-file binding.
	// Independent of the call-edge resolution below, so it runs even when the
	// graph has no unresolved Rust call edges.
	bound := resolveRustModuleImports(g)

	var snap *frameworkNodeSnapshot
	if cands != nil {
		snap = cands.nodes
	}
	idx, extendsResolved := buildRustScopeIndex(g, snap)
	bound += extendsResolved
	if idx == nil {
		return bound
	}

	// Trait-impl override edges bind independently of unresolved call edges,
	// so resolve them before the call-edge early-out below.
	resolved := resolveRustTraitOverrides(g, idx)
	var reindexBatch []graph.EdgeReindex

	// Collect candidate edges (still-unresolved Rust EdgeCalls) plus the
	// caller IDs we need to read receiver type / repo / params off, so
	// the per-edge node lookups collapse to one batch.
	type candEdge struct {
		edge *graph.Edge
	}
	var callCands []candEdge
	fromIDs := make(map[string]struct{})
	callStream := g.EdgesByKind(graph.EdgeCalls)
	if cands != nil {
		callStream = frameworkEdgeSeq(cands.calls)
	}
	for e := range callStream {
		if e == nil {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		if !rustScopeEdgeCandidate(e) {
			continue
		}
		callCands = append(callCands, candEdge{edge: e})
		if e.From != "" {
			fromIDs[e.From] = struct{}{}
		}
	}
	if len(callCands) == 0 {
		return bound + resolved
	}

	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	callerNodes := g.GetNodesByIDs(fromList)

	for _, c := range callCands {
		e := c.edge
		caller := callerNodes[e.From]
		if caller == nil || caller.Language != "rust" {
			continue
		}
		targetID := idx.resolve(e, caller)
		if targetID == "" || targetID == e.To {
			continue
		}
		oldTo := e.To
		e.To = targetID
		e.Origin = idx.lastOrigin
		e.Confidence = idx.lastConfidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, idx.lastConfidence)
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["rust_resolution"] = idx.lastReason
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		resolved++
	}

	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return bound + resolved
}

// resolveRustTraitOverrides binds the unresolved EdgeOverrides the extractor
// emits for `impl Trait for Type` methods (target unresolved::<Trait>.<method>)
// to the trait declaration's method node. The trait may live in another file
// or crate, so the binding runs off the trait-method index rather than the
// caller's file. Returns the number of override edges bound.
func resolveRustTraitOverrides(g graph.Store, idx *rustScopeIndex) int {
	var cands []*graph.Edge
	fromIDs := make(map[string]struct{})
	for e := range g.EdgesByKind(graph.EdgeOverrides) {
		if e == nil || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		if _, _, ok := parseRustOverrideTarget(e.To); !ok {
			continue
		}
		cands = append(cands, e)
		if e.From != "" {
			fromIDs[e.From] = struct{}{}
		}
	}
	if len(cands) == 0 {
		return 0
	}
	fromList := make([]string, 0, len(fromIDs))
	for id := range fromIDs {
		fromList = append(fromList, id)
	}
	fromNodes := g.GetNodesByIDs(fromList)

	bound := 0
	var batch []graph.EdgeReindex
	for _, e := range cands {
		trait, method, _ := parseRustOverrideTarget(e.To)
		from := fromNodes[e.From]
		if from == nil || from.Language != "rust" {
			continue
		}
		target := idx.uniqueTraitMethod(from.RepoPrefix, trait, method)
		if target == "" || target == e.To {
			continue
		}
		oldTo := e.To
		e.To = target
		e.Origin = graph.OriginASTResolved
		e.Confidence = 1.0
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeOverrides, 1.0)
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["rust_resolution"] = "trait_override"
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		bound++
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return bound
}

// parseRustOverrideTarget splits an unresolved::<Trait>.<method> override
// target into its trait + method components.
func parseRustOverrideTarget(to string) (trait, method string, ok bool) {
	name := graph.UnresolvedName(to)
	i := strings.LastIndex(name, ".")
	if i <= 0 || i >= len(name)-1 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
}

// rustScopeEdgeCandidate reports whether an unresolved call edge is one
// this pass can attempt: a path call (Meta["rust_path"] set), a self/Self
// selector call (Meta["rust_recv"] in {self, Self}), or a selector call on
// a typed receiver (Meta["receiver_type"] set) that the generic resolver
// left unresolved because it keys methods by their verbatim (generic)
// receiver. Every other selector call is left to the generic resolver.
func rustScopeEdgeCandidate(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	if p, _ := e.Meta["rust_path"].(string); strings.Contains(p, "::") {
		return true
	}
	if r, _ := e.Meta["rust_recv"].(string); r == "self" || r == "Self" {
		return true
	}
	if rt, _ := e.Meta["receiver_type"].(string); rt != "" {
		return true
	}
	if ex, _ := e.Meta["rust_recv_expr"].(string); strings.HasPrefix(ex, "self.") {
		return true
	}
	return false
}

// rustScopeIndex holds the per-repo method/function lookup tables this
// pass binds against. lastOrigin / lastConfidence / lastReason carry the
// provenance of the most recent resolve() call so the edge-rewrite loop
// can stamp it without resolve() returning a struct.
type rustScopeIndex struct {
	// methodsByOwner: (repo, ownerType) → method nodes of that type.
	methodsByOwner map[rustOwnerKey][]*graph.Node
	// traitTargets resolves a bound path relative to its caller to a concrete
	// trait node. traitMethodsByID keeps direct and inherited methods attached
	// to that concrete identity so same-named traits in different modules do
	// not cross-pollinate through a basename alias.
	traitTargets     *rustTraitTargetIndex
	traitMethodsByID map[string][]*graph.Node
	// typeAliases follows file-scoped use aliases and crate-visible re-export
	// chains before owner and trait lookup. Ambiguous aliases fail closed.
	typeAliases *rustTypeAliasIndex
	// freeFuncsByName: (repo, name) → free function nodes.
	freeFuncsByName map[rustNameKey][]*graph.Node
	// paramsByOwner: caller function/method ID → set of param names,
	// for local-shadows-import precedence.
	paramsByOwner map[string]map[string]struct{}
	// fieldTypesByOwner: (repo, ownerType, fieldName) → declared field type
	// (base name), for walking self.<field>.<field> receiver chains.
	fieldTypesByOwner map[rustFieldKey]string

	lastOrigin     string
	lastConfidence float64
	lastReason     string
}

type rustOwnerKey struct {
	repo  string
	owner string
}

type rustNameKey struct {
	repo string
	name string
}

type rustFieldKey struct {
	repo  string
	owner string
	field string
}

// buildRustScopeIndex walks the graph once and indexes Rust method
// owners, free functions, and caller params. Supertrait edges are resolved
// before the method/function early-out; the returned count includes those
// rewrites even when the graph contains marker traits only. A non-nil snap
// feeds the method / function / param sweeps from the run-wide shared node
// snapshot instead of this pass's own kind scans.
func buildRustScopeIndex(g graph.Store, snap *frameworkNodeSnapshot) (*rustScopeIndex, int) {
	typeAliases := newRustTypeAliasIndex(g)
	traitTargets := newRustTraitTargetIndex(g)
	extendsResolved := resolveRustTraitExtendsWithIndex(g, traitTargets, typeAliases)
	idx := &rustScopeIndex{
		methodsByOwner:    map[rustOwnerKey][]*graph.Node{},
		traitTargets:      traitTargets,
		traitMethodsByID:  map[string][]*graph.Node{},
		typeAliases:       typeAliases,
		freeFuncsByName:   map[rustNameKey][]*graph.Node{},
		paramsByOwner:     map[string]map[string]struct{}{},
		fieldTypesByOwner: map[rustFieldKey]string{},
	}
	any := false
	for _, n := range frameworkKindNodes(g, snap, graph.KindMethod) {
		if n == nil || n.Language != "rust" {
			continue
		}
		if n.Meta != nil {
			if traitDecl, _ := n.Meta["trait_decl"].(string); traitDecl == "true" {
				// Concrete trait identity is indexed after extends resolution;
				// indexing a declaration under its basename here would merge
				// same-named traits from different modules. Synthetic legacy
				// graphs without interface nodes retain exact-owner lookup only.
				if traitTargets == nil {
					if owner := rustBaseTypeName(nodeReceiverType(n)); owner != "" {
						key := rustOwnerKey{repo: n.RepoPrefix, owner: owner}
						idx.methodsByOwner[key] = append(idx.methodsByOwner[key], n)
					}
				}
				any = true
				continue
			}
		}
		owner := nodeReceiverType(n)
		if owner == "" {
			continue
		}
		// Index under the verbatim owner AND a generics/lifetime-stripped
		// base (Candidate<'a> -> Candidate) so a call qualifier or inferred
		// receiver_type that names the base binds to a method whose impl type
		// carries generic args. The module path is kept to avoid cross-module
		// name collisions (io::Error stays io::Error).
		for _, key := range rustOwnerLookupKeys(owner) {
			k := rustOwnerKey{repo: n.RepoPrefix, owner: key}
			idx.methodsByOwner[k] = append(idx.methodsByOwner[k], n)
		}
		any = true
	}
	for _, n := range frameworkKindNodes(g, snap, graph.KindFunction) {
		if n == nil || n.Language != "rust" {
			continue
		}
		idx.freeFuncsByName[rustNameKey{repo: n.RepoPrefix, name: n.Name}] = append(
			idx.freeFuncsByName[rustNameKey{repo: n.RepoPrefix, name: n.Name}], n)
		any = true
	}
	if !any {
		return nil, extendsResolved
	}
	// Params are read lazily-but-once: index every Rust param by its
	// enclosing function/method ID for the shadow check.
	for _, n := range frameworkKindNodes(g, snap, graph.KindParam) {
		owner, rust := rustParameterOwner(n)
		if !rust || owner == "" {
			continue
		}
		set := idx.paramsByOwner[owner]
		if set == nil {
			set = map[string]struct{}{}
			idx.paramsByOwner[owner] = set
		}
		set[n.Name] = struct{}{}
	}
	// Struct fields carry their declared type + owner in Meta, indexed by
	// generics-stripped base names so a self.<field> chain can be walked.
	for n := range g.NodesByKind(graph.KindField) {
		if n == nil || n.Language != "rust" {
			continue
		}
		owner, _ := n.Meta["receiver"].(string)
		ft, _ := n.Meta["field_type"].(string)
		if owner == "" || ft == "" {
			continue
		}
		idx.fieldTypesByOwner[rustFieldKey{
			repo:  n.RepoPrefix,
			owner: rustBaseTypeName(owner),
			field: n.Name,
		}] = rustBaseTypeName(ft)
	}
	inheritRustTraitMethods(g, idx, traitTargets)
	return idx, extendsResolved
}

// resolve returns the target node ID an unresolved Rust call edge should
// bind to, or "" when the call can't be resolved unambiguously. It also
// records the provenance (origin / confidence / reason) of a successful
// binding on the index for the caller to stamp.
func (idx *rustScopeIndex) resolve(e *graph.Edge, caller *graph.Node) string {
	repo := caller.RepoPrefix

	// Selector self/Self call: bind to a method of the caller's owner
	// type. The caller is the enclosing impl method, so its receiver is
	// the impl type.
	if recv, _ := e.Meta["rust_recv"].(string); recv == "self" || recv == "Self" {
		owner := nodeReceiverType(caller)
		if owner == "" {
			return ""
		}
		name := selectorCallName(e.To)
		if name == "" {
			return ""
		}
		if id := idx.uniqueMethod(repo, owner, name); id != "" {
			idx.set(graph.OriginASTResolved, 0.92, "self_receiver")
			return id
		}
		return ""
	}

	// Selector call on a self-rooted field-access receiver
	// (`self.config.line_term.as_byte()`). Walk the field types from the
	// enclosing impl type down the chain, then bind the method on the type
	// the chain lands on.
	if expr, _ := e.Meta["rust_recv_expr"].(string); strings.HasPrefix(expr, "self.") {
		if name := selectorCallName(e.To); name != "" {
			if t := idx.fieldWalk(repo, nodeReceiverType(caller), expr); t != "" {
				if id := idx.uniqueMethod(repo, t, name); id != "" {
					idx.set(graph.OriginASTResolved, 0.82, "field_receiver")
					return id
				}
			}
		}
	}

	// Selector call on a typed variable/param (`mat.buffer()` where
	// `mat: &SinkMatch<'_>`). The generic resolver keys methods by their
	// verbatim receiver ("SinkMatch<'b>"), so a generics-stripped inferred
	// receiver_type ("SinkMatch") misses it. The scope index carries a
	// base-name alias, so bind here when the type owns exactly one such
	// method.
	if rt, _ := e.Meta["receiver_type"].(string); rt != "" {
		if name := selectorCallName(e.To); name != "" {
			if id, aliased, valid := idx.ownerMethod(caller, rt, name); id != "" {
				reason := "receiver_type"
				if aliased {
					reason = "receiver_type_alias"
				}
				idx.set(graph.OriginASTResolved, 0.88, reason)
				return id
			} else if !valid || aliased {
				// An ambiguous alias, or a qualified alias whose destination did
				// not match a graph owner, must not degrade to basename guessing.
				return ""
			}
			// A parameter whose declared type is a generic (`matcher: M`)
			// has no concrete method owner. If the enclosing function's
			// effective type parameters constrain M to a trait that declares
			// this method, bind to that trait declaration. The helper scans
			// every bound and returns a target only when the declaration is
			// unique across the full bound set.
			if id := idx.uniqueGenericBoundTraitMethod(repo, caller, rustBaseTypeName(rt), name); id != "" {
				idx.set(graph.OriginASTResolved, 0.86, "generic_trait_bound")
				return id
			}
		}
	}

	path, _ := e.Meta["rust_path"].(string)
	if !strings.Contains(path, "::") {
		return ""
	}
	segments := strings.Split(path, "::")
	last := segments[len(segments)-1]
	if last == "" {
		return ""
	}
	qualifier := segments[len(segments)-2]

	switch {
	case qualifier == "Self":
		// Self::method() — same binding as the self selector case.
		owner := nodeReceiverType(caller)
		if owner == "" {
			return ""
		}
		if id := idx.uniqueMethod(repo, owner, last); id != "" {
			idx.set(graph.OriginASTResolved, 0.92, "self_path")
			return id
		}
		return ""

	case isRustTypeName(qualifier):
		// Type::method() — bind to a method whose owner type is the
		// qualifier. File-local aliases and public re-export chains are
		// canonicalised before owner lookup.
		if id, aliased, valid := idx.ownerMethod(caller, qualifier, last); id != "" {
			reason := "impl_owner"
			if aliased {
				reason = "impl_owner_alias"
			}
			idx.set(graph.OriginASTResolved, 0.9, reason)
			return id
		} else if !valid || aliased {
			return ""
		}
		// Ambiguous by type name alone — the same type name is defined in
		// more than one crate/module (e.g. grep::regex::RegexMatcherBuilder
		// and grep::pcre2::RegexMatcherBuilder). Disambiguate with the
		// qualified path's crate/module segments: bind to the candidate
		// whose file lives under a directory named by a path segment.
		if id := idx.methodByPathSegments(repo, qualifier, last, segments); id != "" {
			idx.set(graph.OriginASTResolved, 0.9, "impl_owner_path")
			return id
		}
		// Still ambiguous, and the call named no disambiguating path segment
		// (bare `RegexMatcherBuilder::new()`). Prefer the candidate defined in
		// the caller's own crate: a same-crate associated-function call almost
		// always means the same-crate type.
		if id := idx.methodBySameCrate(repo, qualifier, last, caller.FilePath); id != "" {
			idx.set(graph.OriginASTResolved, 0.85, "impl_owner_crate")
			return id
		}
		return ""

	default:
		// Module path: crate::/super::/self::/<module>::func(). Gortex
		// doesn't model the module tree, so bind the trailing segment to
		// a free function in the same repo, preferring locality. Skipped
		// when a same-named caller param shadows the import.
		if idx.callerShadows(e.From, last) {
			return ""
		}
		if id := idx.uniqueFreeFunc(repo, last, caller.FilePath); id != "" {
			idx.set(graph.OriginASTInferred, 0.75, "module_path")
			return id
		}
		return ""
	}
}

func (idx *rustScopeIndex) set(origin string, conf float64, reason string) {
	idx.lastOrigin = origin
	idx.lastConfidence = conf
	idx.lastReason = reason
}

// ownerMethod resolves a possibly aliased Rust owner and returns its unique
// method. Qualified alias destinations are matched against file-path segments
// before any basename lookup, preventing an external or ambiguous alias from
// silently binding to an unrelated local type.
func (idx *rustScopeIndex) ownerMethod(caller *graph.Node, owner, name string) (id string, aliased, valid bool) {
	if caller == nil || owner == "" || name == "" {
		return "", false, false
	}
	ownerPath := rustBaseTypeName(owner)
	if idx.typeAliases != nil {
		var ok bool
		ownerPath, aliased, ok = idx.typeAliases.resolve(caller, ownerPath)
		if !ok || ownerPath == "" {
			return "", aliased, false
		}
	}
	canonicalOwner := rustUseBindingName(ownerPath)
	if canonicalOwner == "" {
		return "", aliased, false
	}
	if aliased && strings.Contains(ownerPath, "::") {
		segments := strings.Split(ownerPath, "::")
		if id := idx.methodByPathSegments(caller.RepoPrefix, canonicalOwner, name, segments); id != "" {
			return id, true, true
		}
		// A qualified alias carries stronger evidence than a basename. If
		// its path cannot identify a matching owner, fail closed.
		return "", true, true
	}
	return idx.uniqueMethod(caller.RepoPrefix, canonicalOwner, name), aliased, true
}

// uniqueMethod returns the ID of the single method named `name` owned by
// (repo, owner), or "" when there is no match or the choice is
// ambiguous (more than one).
func (idx *rustScopeIndex) uniqueMethod(repo, owner, name string) string {
	cands := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
	var hit string
	for _, m := range cands {
		if m.Name != name {
			continue
		}
		if hit != "" && hit != m.ID {
			return "" // ambiguous
		}
		hit = m.ID
	}
	return hit
}

// methodByPathSegments disambiguates a Type::method call whose type name is
// defined in more than one crate/module by matching the qualified path's
// crate/module segments (grep::regex::Foo::bar -> a candidate whose file
// lives under a `regex` directory) against each candidate's file path.
// Returns the ID only when exactly one candidate matches a path segment.
func (idx *rustScopeIndex) methodByPathSegments(repo, owner, name string, segments []string) string {
	cands := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
	var hit string
	for _, m := range cands {
		if m.Name != name {
			continue
		}
		for _, seg := range segments {
			if seg == "" || seg == owner || seg == name {
				continue
			}
			if rustFileMatchesModuleSegment(m.FilePath, seg) {
				if hit != "" && hit != m.ID {
					return "" // more than one crate/module matched
				}
				hit = m.ID
				break
			}
		}
	}
	return hit
}

func rustFileMatchesModuleSegment(filePath, segment string) bool {
	filePath = strings.TrimSpace(strings.ReplaceAll(filePath, "\\", "/"))
	segment = strings.Trim(strings.TrimSpace(segment), "/:")
	if filePath == "" || segment == "" {
		return false
	}
	return strings.Contains(filePath, "/"+segment+"/") ||
		strings.HasSuffix(filePath, "/"+segment+".rs") ||
		strings.HasSuffix(filePath, "/"+segment+"/mod.rs") ||
		filePath == segment+".rs"
}

// methodBySameCrate disambiguates a `Type::method` call that names no
// disambiguating path segment by preferring the candidate defined in the
// caller's own crate. Returns the ID only when exactly one candidate lives
// in that crate.
func (idx *rustScopeIndex) methodBySameCrate(repo, owner, name, callerFile string) string {
	callerCrate := rustCrateOf(callerFile)
	if callerCrate == "" {
		return ""
	}
	cands := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
	var hit string
	for _, m := range cands {
		if m.Name != name {
			continue
		}
		if rustCrateOf(m.FilePath) != callerCrate {
			continue
		}
		if hit != "" && hit != m.ID {
			return "" // more than one candidate in the caller's crate
		}
		hit = m.ID
	}
	return hit
}

// fieldWalk resolves the type a self-rooted field-access receiver lands on.
// Given the enclosing impl type (`Searcher`) and a receiver expression
// (`self.config.line_term`), it walks each field via the field-type index —
// Searcher.config -> Config, Config.line_term -> LineTerminator — and
// returns the final type, or "" if any hop is unknown or ambiguous.
func (idx *rustScopeIndex) fieldWalk(repo, implType, expr string) string {
	t := rustBaseTypeName(implType)
	if t == "" {
		return ""
	}
	fields := strings.Split(strings.TrimPrefix(expr, "self."), ".")
	for _, f := range fields {
		if f == "" {
			return ""
		}
		next := idx.fieldTypesByOwner[rustFieldKey{repo: repo, owner: t, field: f}]
		if next == "" {
			return ""
		}
		t = next
	}
	return t
}

// rustCrateOf returns a stable identifier for the crate a Rust source file
// belongs to: the path up to (and excluding) the "/src/" segment that marks
// a cargo crate root (crates/regex/src/matcher.rs -> "crates/regex",
// myproj/src/lib.rs -> "myproj"). Files with no "/src/" segment — flat
// scripts, test-dir files, or synthetic fixtures — have no determinable
// crate and return "", so the same-crate tiebreaker stays conservative and
// never guesses across an unknown boundary.
func rustCrateOf(path string) string {
	if i := strings.Index(path, "/src/"); i >= 0 {
		return path[:i]
	}
	return ""
}

// uniqueTraitMethod returns the ID of the single trait-declaration method
// named `name` owned by trait `owner` in repo, or "" on no match or
// ambiguity. Only nodes marked Meta["trait_decl"]="true" qualify, so an
// inherent method on a same-named type is never mistaken for the trait's.
func (idx *rustScopeIndex) uniqueTraitMethod(repo, owner, name string) string {
	cands := idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
	var hit string
	for _, m := range cands {
		if m.Name != name || m.Meta == nil {
			continue
		}
		if td, _ := m.Meta["trait_decl"].(string); td != "true" {
			continue
		}
		if hit != "" && hit != m.ID {
			return ""
		}
		hit = m.ID
	}
	return hit
}

// uniqueGenericBoundTraitMethod returns the single trait declaration method
// named by all bounds on generic parameter param. It deliberately scans the raw
// candidate lists rather than composing uniqueTraitMethod calls: a duplicated
// declaration for one bound must keep the call unresolved even if another bound
// happens to have one unique declaration.
func (idx *rustScopeIndex) uniqueGenericBoundTraitMethod(repo string, caller *graph.Node, param, name string) string {
	if caller == nil || caller.Meta == nil || param == "" || name == "" {
		return ""
	}
	owners := rustTypeParamTraitNames(caller.Meta["type_params"], param)
	if len(owners) == 0 {
		return ""
	}
	var hit string
	for _, owner := range owners {
		var methods []*graph.Node
		if idx.typeAliases != nil {
			resolvedOwner, _, ok := idx.typeAliases.resolve(caller, owner)
			if !ok {
				return ""
			}
			owner = resolvedOwner
		}
		if idx.traitTargets != nil {
			traitID := idx.traitTargets.resolve(caller, owner)
			if traitID == "" {
				// A qualified path may name an external trait. Never degrade it
				// to a same-named local basename merely because one exists.
				continue
			}
			methods = idx.traitMethodsByID[traitID]
		} else {
			// Compatibility for small synthetic indexes that predate concrete
			// trait nodes: use only the exact parsed bound key. An explicit
			// crate:: root is proven local, so its crate-relative exact key is
			// also safe; arbitrary qualified paths never degrade to basename.
			methods = idx.methodsByOwner[rustOwnerKey{repo: repo, owner: owner}]
			if len(methods) == 0 && strings.HasPrefix(owner, "crate::") {
				crateRelative := strings.TrimPrefix(owner, "crate::")
				methods = idx.methodsByOwner[rustOwnerKey{repo: repo, owner: crateRelative}]
			}
		}
		for _, method := range methods {
			if method == nil || method.Name != name || method.Meta == nil {
				continue
			}
			if traitDecl, _ := method.Meta["trait_decl"].(string); traitDecl != "true" {
				continue
			}
			if hit != "" && hit != method.ID {
				return ""
			}
			hit = method.ID
		}
	}
	return hit
}

// uniqueFreeFunc returns the ID of a free function named `name` in repo,
// preferring a same-file candidate, then a same-directory candidate,
// then a unique candidate overall. Returns "" when nothing matches or
// the choice is ambiguous (more than one across different files with no
// locality tie-break).
func (idx *rustScopeIndex) uniqueFreeFunc(repo, name, callerFile string) string {
	cands := idx.freeFuncsByName[rustNameKey{repo: repo, name: name}]
	if len(cands) == 0 {
		return ""
	}
	if len(cands) == 1 {
		return cands[0].ID
	}
	callerDir := rustParentDir(callerFile)
	var election rustFreeFuncElection
	for _, n := range cands {
		election.add(n, callerFile, callerDir)
	}
	return election.target()
}

// rustFreeFuncElection counts at most two candidates per locality tier.
type rustFreeFuncElection struct {
	all, file, dir struct {
		count int
		id    string
	}
}

func (e *rustFreeFuncElection) add(n *graph.Node, callerFile, callerDir string) {
	e.all.count, e.all.id = min(2, e.all.count+1), n.ID
	if n.FilePath == callerFile {
		e.file.count, e.file.id = min(2, e.file.count+1), n.ID
	}
	if rustParentDir(n.FilePath) == callerDir {
		e.dir.count, e.dir.id = min(2, e.dir.count+1), n.ID
	}
}

func (e *rustFreeFuncElection) target() string {
	switch {
	case e.all.count == 1:
		return e.all.id
	case e.file.count == 1:
		return e.file.id
	case e.file.count == 0 && e.dir.count == 1:
		return e.dir.id
	}
	return ""
}

func rustParameterOwner(n *graph.Node) (string, bool) {
	if n == nil || n.Language != "rust" {
		return "", false
	}
	return enclosingFunctionForBinding(n.ID), true
}

// callerShadows reports whether the calling function/method declares a
// parameter named `name` — a local binding that shadows an import of
// the same identifier under Rust's name-resolution rules.
func (idx *rustScopeIndex) callerShadows(callerID, name string) bool {
	set := idx.paramsByOwner[callerID]
	if set == nil {
		return false
	}
	_, ok := set[name]
	return ok
}

// selectorCallName extracts the method name from a selector-call
// placeholder target of the form `unresolved::*.<name>` (or the
// per-repo `<repo>::unresolved::*.<name>` form).
func selectorCallName(to string) string {
	name := graph.UnresolvedName(to)
	if name == "" {
		return ""
	}
	name = strings.TrimPrefix(name, "*.")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// isRustTypeName reports whether s looks like a Rust type path qualifier
// (UpperCamelCase) rather than a module/path keyword. Crate-relative
// keywords (crate/super/self) and lowercase module names are not types.
func isRustTypeName(s string) bool {
	switch s {
	case "", "crate", "super", "self", "Self":
		return false
	}
	c := s[0]
	return c >= 'A' && c <= 'Z'
}

// rustParentDir returns the slash-separated parent directory of a graph
// file path. Graph paths are slash-normalised, so a plain byte scan is
// correct on every OS.
func rustParentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// rustOwnerLookupKeys returns the keys a method's verbatim owner type should
// be indexed under: the verbatim text plus a generics/lifetime/ref-stripped
// base (module path kept). "Candidate<'a>" -> ["Candidate<'a>", "Candidate"];
// "io::Error" -> ["io::Error"]; "Foo" -> ["Foo"].
func rustOwnerLookupKeys(owner string) []string {
	keys := []string{owner}
	if base := rustBaseTypeName(owner); base != "" && base != owner {
		keys = append(keys, base)
	}
	return keys
}

// rustBaseTypeName strips references, a leading lifetime and generic args from
// a verbatim Rust type, keeping the module path: "&'a mut Candidate<'a>" ->
// "Candidate", "io::Error" -> "io::Error".
func rustBaseTypeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "&mut ")
	s = strings.TrimPrefix(s, "&")
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") {
		if i := strings.IndexByte(s, ' '); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
			s = strings.TrimPrefix(s, "mut ")
			s = strings.TrimSpace(s)
		}
	}
	if i := strings.Index(s, "<"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
