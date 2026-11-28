package graph

import (
	"sort"
	"strings"
)

// RepoLanguageFileCount is a compact, metadata-free projection used to size
// and gate semantic enrichment. Count is the number of non-content nodes with
// the same repository, source file, and language.
type RepoLanguageFileCount struct {
	RepoPrefix string
	FilePath   string
	Language   string
	Count      int
}

// RepoLanguageFileCountReader returns language/file node counts for all
// requested repository prefixes in one backend operation. Implementations must
// filter content sections before aggregation and must not materialize Node.Meta.
type RepoLanguageFileCountReader interface {
	RepoLanguageFileCounts(repoPrefixes []string) []RepoLanguageFileCount
}

// RepoLanguageCountReader returns non-content node counts grouped by repository
// and language in one backend operation. Empty languages are omitted.
type RepoLanguageCountReader interface {
	RepoLanguageCounts(repoPrefixes []string) map[string]map[string]int
}

// RepoNodeKindIDReader projects only node IDs matching the requested repository
// prefixes and kinds. Implementations must evaluate both sets in one backend
// operation rather than issuing one query per repository.
type RepoNodeKindIDReader interface {
	RepoNodeIDsByKinds(repoPrefixes []string, kinds []NodeKind) []string
}

// RepoFilePathReader projects file paths owned by one repository/workspace and
// matching a language or extension predicate. Production implementations push
// every predicate into the backend and return only paths, never full nodes.
type RepoFilePathReader interface {
	RepoFilePaths(repoPrefix, workspaceID string, languages, extensions []string) []string
}

// RepoMetaNodeReader returns nodes of the requested kinds carrying one metadata
// key in a repository/workspace scope. This is for the small set of passes that
// need selected metadata without scanning every node of each kind globally.
type RepoMetaNodeReader interface {
	RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID string, kinds []NodeKind, metaKey string) []*Node
}

// ReadRepoFilePaths selects the exact production projection. The adapter
// fallback performs one required repository read, never a global kind scan.
func ReadRepoFilePaths(s Store, repoPrefix, workspaceID string, languages, extensions []string) []string {
	if s == nil {
		return nil
	}
	if reader, ok := s.(RepoFilePathReader); ok {
		return reader.RepoFilePaths(repoPrefix, workspaceID, languages, extensions)
	}
	seen := make(map[string]struct{})
	var out []string
	for _, node := range s.GetRepoNodes(repoPrefix) {
		if node == nil || node.Kind != KindFile || !projectionNodeMatches(node, workspaceID, languages, extensions) {
			continue
		}
		path := node.FilePath
		if path == "" {
			path = node.ID
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// ReadRepoNodesByKindsWithMetaKey selects reader nodes in one backend call (or
// one required repository read for adapters), never one lookup per node.
func ReadRepoNodesByKindsWithMetaKey(s Store, repoPrefix, workspaceID string, kinds []NodeKind, metaKey string) []*Node {
	if s == nil || len(kinds) == 0 || metaKey == "" {
		return nil
	}
	if reader, ok := s.(RepoMetaNodeReader); ok {
		return reader.RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID, kinds, metaKey)
	}
	wantedKinds := make(map[NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	var out []*Node
	for _, node := range s.GetRepoNodes(repoPrefix) {
		if node == nil || (workspaceID != "" && node.WorkspaceID != workspaceID) || node.Meta == nil {
			continue
		}
		if _, ok := wantedKinds[node.Kind]; !ok {
			continue
		}
		if _, ok := node.Meta[metaKey]; ok {
			out = append(out, node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func projectionNodeMatches(node *Node, workspaceID string, languages, extensions []string) bool {
	if node == nil || (workspaceID != "" && node.WorkspaceID != workspaceID) {
		return false
	}
	path := node.FilePath
	if path == "" {
		path = node.ID
	}
	return ProjectionMatches(path, node.Language, languages, extensions)
}

// ProjectionMatches applies the in-memory file projection rule. The SQLite
// RepoFilePaths projection evaluates the Spring language/extension union in SQL.
func ProjectionMatches(path, language string, languages, extensions []string) bool {
	if len(languages) == 0 && len(extensions) == 0 {
		return true
	}
	language = strings.ToLower(language)
	for _, candidate := range languages {
		if language == strings.ToLower(candidate) {
			return true
		}
	}
	path = strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	for _, extension := range extensions {
		extension = strings.ToLower(extension)
		if extension != "" && !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		if extension != "" && strings.HasSuffix(path, extension) {
			return true
		}
	}
	return false
}

// RepoEdgeRow preserves the owning source repository alongside an edge. Edge
// itself intentionally carries no repository field; callers that batch several
// repositories need this projection to partition one backend read without a
// follow-up GetNode for every source endpoint.
type RepoEdgeRow struct {
	RepoPrefix string
	Edge       *Edge
}

// RepoEdgeKindReader returns edges whose source node belongs to any requested
// repository and whose kind is requested. Implementations evaluate both sets in
// one backend operation; in particular, they must not issue one GetRepoEdges
// query per repository.
type RepoEdgeKindReader interface {
	RepoEdgesByKinds(repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow
}

// RepoCapabilityEdge is the complete base-edge projection needed to derive
// reads_env, executes_process, and accesses_field edges. Reads and writes are
// included only when their target is currently a field; payload columns such as
// confidence, origin, and Meta are deliberately absent.
type RepoCapabilityEdge struct {
	RepoPrefix string
	Identity   EdgeIdentity
}

// RepoCapabilityEdgeScanner streams bounded pages of capability-producing base
// edges owned by the requested repositories. A nil repoPrefixes slice requests
// the whole graph; a non-nil empty slice is an exact empty scope. Implementations
// must close each backend cursor before invoking yield so the callback may safely
// re-enter the Store. Page order is unspecified; consumers that select
// provenance must apply their own deterministic ordering rule.
type RepoCapabilityEdgeScanner interface {
	ScanRepoCapabilityEdges(repoPrefixes []string, pageSize int, yield func([]RepoCapabilityEdge) bool)
}

// Four thousand identities stay comfortably bounded while avoiding thousands
// of prepare/step/finalize cycles on multi-million-edge cold stores.
const defaultRepoCapabilityEdgePageSize = 4096

// ScanRepoCapabilityEdges uses the compact production projection when
// available. The adapter fallback preserves exact behavior for in-memory and
// decorated stores; it may materialize full edges because those stores do not
// own a backend cursor or duplicated SQLite payload buffers.
func ScanRepoCapabilityEdges(
	s Store,
	repoPrefixes []string,
	pageSize int,
	yield func([]RepoCapabilityEdge) bool,
) {
	if s == nil || yield == nil || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return
	}
	if pageSize <= 0 {
		pageSize = defaultRepoCapabilityEdgePageSize
	}
	if scanner, ok := s.(RepoCapabilityEdgeScanner); ok {
		scanner.ScanRepoCapabilityEdges(repoPrefixes, pageSize, yield)
		return
	}

	// Adapter stores do not own a cursor projection. Their graph is already
	// resident, so preserve whole-graph semantics (including legacy empty-prefix
	// nodes) with the constant number of kind walks the old pass performed.
	kinds := []EdgeKind{EdgeReadsConfig, EdgeReads, EdgeWrites, EdgeCalls}
	var rows []RepoEdgeRow
	if repoPrefixes != nil {
		rows = ReadRepoEdgesByKinds(s, repoPrefixes, kinds)
	} else {
		var candidates []*Edge
		if scanner, ok := s.(EdgesByKindsScanner); ok {
			for edge := range scanner.EdgesByKinds(kinds) {
				if edge != nil {
					candidates = append(candidates, edge)
				}
			}
		} else {
			for _, kind := range kinds {
				for edge := range s.EdgesByKind(kind) {
					if edge != nil {
						candidates = append(candidates, edge)
					}
				}
			}
		}
		rows = make([]RepoEdgeRow, 0, len(candidates))
		for _, edge := range candidates {
			// The unscoped consumer needs only deterministic identity order; a
			// repository lookup for every source would recreate the very node
			// materialization this projection removes.
			rows = append(rows, RepoEdgeRow{Edge: edge})
		}
		sortRepoEdgeRows(rows)
	}
	fieldTargetIDs := make([]string, 0)
	seenTargets := make(map[string]struct{})
	for _, row := range rows {
		edge := row.Edge
		if edge == nil || (edge.Kind != EdgeReads && edge.Kind != EdgeWrites) {
			continue
		}
		if _, seen := seenTargets[edge.To]; seen {
			continue
		}
		seenTargets[edge.To] = struct{}{}
		fieldTargetIDs = append(fieldTargetIDs, edge.To)
	}
	fieldTargets := s.GetNodesByIDs(fieldTargetIDs)

	page := make([]RepoCapabilityEdge, 0, pageSize)
	for _, row := range rows {
		edge := row.Edge
		if edge == nil {
			continue
		}
		if edge.Kind == EdgeReads || edge.Kind == EdgeWrites {
			target := fieldTargets[edge.To]
			if target == nil || target.Kind != KindField {
				continue
			}
		}
		page = append(page, RepoCapabilityEdge{
			RepoPrefix: row.RepoPrefix,
			Identity:   EdgeIdentityFor(edge),
		})
		if len(page) == pageSize {
			if !yield(page) {
				return
			}
			page = make([]RepoCapabilityEdge, 0, pageSize)
		}
	}
	if len(page) > 0 {
		yield(page)
	}
}

// ReadRepoLanguageFileCounts uses the compact capability when available. The
// aggregate fallback keeps adapter stores functional without loading nodes or
// metadata; production Graph and SQLite stores implement the exact file-aware
// capability below.
func ReadRepoLanguageFileCounts(s Store, repoPrefixes []string) []RepoLanguageFileCount {
	if len(repoPrefixes) == 0 {
		return nil
	}
	if r, ok := s.(RepoLanguageFileCountReader); ok {
		return r.RepoLanguageFileCounts(repoPrefixes)
	}

	counts := ReadRepoLanguageCounts(s, repoPrefixes)
	out := make([]RepoLanguageFileCount, 0)
	for repoPrefix, byLanguage := range counts {
		for language, count := range byLanguage {
			if language == "" || count <= 0 {
				continue
			}
			out = append(out, RepoLanguageFileCount{
				RepoPrefix: repoPrefix,
				Language:   language,
				Count:      count,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepoPrefix != out[j].RepoPrefix {
			return out[i].RepoPrefix < out[j].RepoPrefix
		}
		return out[i].Language < out[j].Language
	})
	return out
}

// ReadRepoLanguageCounts uses the node-only capability when available. The
// single RepoStats fallback is reserved for adapter stores; production Graph
// and SQLite stores avoid RepoStats and therefore never scan edge counts here.
func ReadRepoLanguageCounts(s Store, repoPrefixes []string) map[string]map[string]int {
	if len(repoPrefixes) == 0 {
		return nil
	}
	if r, ok := s.(RepoLanguageCountReader); ok {
		return r.RepoLanguageCounts(repoPrefixes)
	}

	wanted := stringSet(repoPrefixes)
	out := make(map[string]map[string]int, len(wanted))
	for repoPrefix, stats := range s.RepoStats() {
		if _, ok := wanted[repoPrefix]; !ok {
			continue
		}
		byLanguage := make(map[string]int, len(stats.ByLanguage))
		for language, count := range stats.ByLanguage {
			if language != "" && count > 0 {
				byLanguage[language] = count
			}
		}
		if len(byLanguage) > 0 {
			out[repoPrefix] = byLanguage
		}
	}
	return out
}

// ReadRepoNodeIDsByKinds uses the set-oriented capability when available. Its
// fallback performs one predicate read per requested kind (never per repo), so
// adapters remain correct without an N+1 repository loop.
func ReadRepoNodeIDsByKinds(s Store, repoPrefixes []string, kinds []NodeKind) []string {
	if len(repoPrefixes) == 0 || len(kinds) == 0 {
		return nil
	}
	if r, ok := s.(RepoNodeKindIDReader); ok {
		return r.RepoNodeIDsByKinds(repoPrefixes, kinds)
	}

	wantedRepos := stringSet(repoPrefixes)
	wantedKinds := make(map[NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	ids := make(map[string]struct{})
	for kind := range wantedKinds {
		for n := range s.NodesByKind(kind) {
			if n == nil {
				continue
			}
			if _, ok := wantedRepos[n.RepoPrefix]; ok {
				ids[n.ID] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ReadRepoEdgesByKinds uses the set-oriented repository/kind projection when
// available. The adapter fallback performs a constant number of kind scans and
// one batched source-node lookup, never a repository or edge-shaped point-query
// loop. Production Graph and SQLite stores implement the capability directly.
func ReadRepoEdgesByKinds(s Store, repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow {
	if s == nil || len(repoPrefixes) == 0 || len(kinds) == 0 {
		return nil
	}
	if r, ok := s.(RepoEdgeKindReader); ok {
		return r.RepoEdgesByKinds(repoPrefixes, kinds)
	}

	wantedRepos := stringSet(repoPrefixes)
	wantedKinds := make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	var candidates []*Edge
	if scanner, ok := s.(EdgesByKindsScanner); ok {
		for edge := range scanner.EdgesByKinds(kinds) {
			if edge != nil {
				candidates = append(candidates, edge)
			}
		}
	} else {
		for kind := range wantedKinds {
			for edge := range s.EdgesByKind(kind) {
				if edge != nil {
					candidates = append(candidates, edge)
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sourceIDs := make([]string, 0, len(candidates))
	for _, edge := range candidates {
		sourceIDs = append(sourceIDs, edge.From)
	}
	sources := s.GetNodesByIDs(sourceIDs)
	out := make([]RepoEdgeRow, 0, len(candidates))
	for _, edge := range candidates {
		source := sources[edge.From]
		if source == nil {
			continue
		}
		if _, ok := wantedRepos[source.RepoPrefix]; !ok {
			continue
		}
		out = append(out, RepoEdgeRow{RepoPrefix: source.RepoPrefix, Edge: edge})
	}
	sortRepoEdgeRows(out)
	return out
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

// visitRepoProjectionNodes walks only the requested repository buckets. Empty
// prefix nodes are intentionally absent from byRepo, so that exact bucket is
// read from the shard's node map without materializing a whole-graph slice.
func (g *Graph) visitRepoProjectionNodes(repoPrefixes []string, visit func(*Node)) {
	wanted := stringSet(repoPrefixes)
	if len(wanted) == 0 {
		return
	}
	_, wantEmpty := wanted[""]
	for _, shard := range g.shards {
		shard.mu.RLock()
		if wantEmpty {
			for _, n := range shard.nodes {
				if n != nil && n.RepoPrefix == "" {
					visit(n)
				}
			}
		}
		for repoPrefix := range wanted {
			if repoPrefix == "" {
				continue
			}
			for _, n := range shard.byRepo[repoPrefix] {
				if n != nil {
					visit(n)
				}
			}
		}
		shard.mu.RUnlock()
	}
}

// RepoLanguageFileCounts implements RepoLanguageFileCountReader without
// allocating Node copies or reading any metadata beyond already-resident Graph
// nodes.
func (g *Graph) RepoLanguageFileCounts(repoPrefixes []string) []RepoLanguageFileCount {
	type key struct {
		repoPrefix string
		filePath   string
		language   string
	}
	counts := make(map[key]int)
	g.visitRepoProjectionNodes(repoPrefixes, func(n *Node) {
		if n.Language == "" || IsContentNode(n) {
			return
		}
		// Module nodes describe a manifest's DEPENDENCIES, not the repo's own
		// source; counting them let a Cargo.toml or package-lock.json vouch
		// for languages the repo contains no code in. Mirrors the SQLite
		// projection's kind filter.
		if n.Kind == KindModule {
			return
		}
		counts[key{n.RepoPrefix, n.FilePath, n.Language}]++
	})
	out := make([]RepoLanguageFileCount, 0, len(counts))
	for k, count := range counts {
		out = append(out, RepoLanguageFileCount{
			RepoPrefix: k.repoPrefix,
			FilePath:   k.filePath,
			Language:   k.language,
			Count:      count,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RepoPrefix != out[j].RepoPrefix {
			return out[i].RepoPrefix < out[j].RepoPrefix
		}
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Language < out[j].Language
	})
	return out
}

// RepoLanguageCounts implements RepoLanguageCountReader with one shard walk.
func (g *Graph) RepoLanguageCounts(repoPrefixes []string) map[string]map[string]int {
	out := make(map[string]map[string]int)
	g.visitRepoProjectionNodes(repoPrefixes, func(n *Node) {
		if n.Language == "" || IsContentNode(n) {
			return
		}
		byLanguage := out[n.RepoPrefix]
		if byLanguage == nil {
			byLanguage = make(map[string]int)
			out[n.RepoPrefix] = byLanguage
		}
		byLanguage[n.Language]++
	})
	return out
}

// RepoNodeIDsByKinds implements RepoNodeKindIDReader with one shard walk over
// the requested repository buckets.
func (g *Graph) RepoNodeIDsByKinds(repoPrefixes []string, kinds []NodeKind) []string {
	wantedKinds := make(map[NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	if len(wantedKinds) == 0 {
		return nil
	}
	var out []string
	g.visitRepoProjectionNodes(repoPrefixes, func(n *Node) {
		if _, ok := wantedKinds[n.Kind]; ok {
			out = append(out, n.ID)
		}
	})
	sort.Strings(out)
	return out
}

// RepoEdgesByKinds implements RepoEdgeKindReader from the repository and
// source-adjacency buckets already resident in each shard. It walks each
// requested bucket once and never performs endpoint lookups.
func (g *Graph) RepoEdgesByKinds(repoPrefixes []string, kinds []EdgeKind) []RepoEdgeRow {
	wantedRepos := stringSet(repoPrefixes)
	wantedKinds := make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	if len(wantedRepos) == 0 || len(wantedKinds) == 0 {
		return nil
	}
	var out []RepoEdgeRow
	for _, shard := range g.shards {
		shard.mu.RLock()
		for repoPrefix := range wantedRepos {
			if repoPrefix == "" {
				for id, node := range shard.nodes {
					if node == nil || node.RepoPrefix != "" {
						continue
					}
					for _, edge := range shard.outEdges[id] {
						if edge != nil {
							if _, ok := wantedKinds[edge.Kind]; ok {
								out = append(out, RepoEdgeRow{RepoPrefix: repoPrefix, Edge: edge})
							}
						}
					}
				}
				continue
			}
			for _, node := range shard.byRepo[repoPrefix] {
				if node == nil {
					continue
				}
				for _, edge := range shard.outEdges[node.ID] {
					if edge != nil {
						if _, ok := wantedKinds[edge.Kind]; ok {
							out = append(out, RepoEdgeRow{RepoPrefix: repoPrefix, Edge: edge})
						}
					}
				}
			}
		}
		shard.mu.RUnlock()
	}
	sortRepoEdgeRows(out)
	return out
}

// RepoFilePaths implements RepoFilePathReader from the repository buckets
// already resident in memory, without a workspace-wide KindFile snapshot.
func (g *Graph) RepoFilePaths(repoPrefix, workspaceID string, languages, extensions []string) []string {
	seen := make(map[string]struct{})
	var out []string
	g.visitRepoProjectionNodes([]string{repoPrefix}, func(node *Node) {
		if node.Kind != KindFile || !projectionNodeMatches(node, workspaceID, languages, extensions) {
			return
		}
		path := node.FilePath
		if path == "" {
			path = node.ID
		}
		if _, duplicate := seen[path]; duplicate {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	})
	sort.Strings(out)
	return out
}

// RepoNodesByKindsWithMetaKey implements RepoMetaNodeReader from the exact
// repository bucket and returns only matching full rows.
func (g *Graph) RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID string, kinds []NodeKind, metaKey string) []*Node {
	wantedKinds := make(map[NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	if len(wantedKinds) == 0 || metaKey == "" {
		return nil
	}
	var out []*Node
	g.visitRepoProjectionNodes([]string{repoPrefix}, func(node *Node) {
		if node == nil || (workspaceID != "" && node.WorkspaceID != workspaceID) || node.Meta == nil {
			return
		}
		if _, ok := wantedKinds[node.Kind]; !ok {
			return
		}
		if _, ok := node.Meta[metaKey]; ok {
			out = append(out, node)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortRepoEdgeRows(rows []RepoEdgeRow) {
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.RepoPrefix != right.RepoPrefix {
			return left.RepoPrefix < right.RepoPrefix
		}
		if left.Edge == nil || right.Edge == nil {
			return left.Edge != nil
		}
		if left.Edge.From != right.Edge.From {
			return left.Edge.From < right.Edge.From
		}
		if left.Edge.To != right.Edge.To {
			return left.Edge.To < right.Edge.To
		}
		if left.Edge.Kind != right.Edge.Kind {
			return left.Edge.Kind < right.Edge.Kind
		}
		if left.Edge.FilePath != right.Edge.FilePath {
			return left.Edge.FilePath < right.Edge.FilePath
		}
		return left.Edge.Line < right.Edge.Line
	})
}

var (
	_ RepoLanguageFileCountReader = (*Graph)(nil)
	_ RepoLanguageCountReader     = (*Graph)(nil)
	_ RepoNodeKindIDReader        = (*Graph)(nil)
	_ RepoEdgeKindReader          = (*Graph)(nil)
	_ RepoFilePathReader          = (*Graph)(nil)
	_ RepoMetaNodeReader          = (*Graph)(nil)
)
