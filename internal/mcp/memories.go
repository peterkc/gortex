package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// maxMemoriesCap is the soft ceiling on stored memories per repo
// scope. Trimming honours pinned + high-importance memories. Matches
// the prior gob.gz cap.
const maxMemoriesCap = 10000

// memoryManager owns the cross-session development-memory store.
// It mirrors notesManager structurally (same persistence + filter
// shape) but its entries have no SessionID — every memory is
// workspace-scoped and durable across sessions, so the store
// compounds the longer a team uses Gortex.
//
// Memories live alongside the graph as a separate, persistent
// side-store backed by the SQLite sidecar DB. The in-memory slice +
// scorers are unchanged from the gob.gz era; only the persistence
// layer changed. A nil sidecar yields an in-memory-only manager
// (test fixtures, single-shot CLI calls).
type memoryManager struct {
	mu      sync.Mutex
	store   persistence.MemoryStore
	sidecar memoryRecords
	repoKey string
}

// newMemoryManager constructs a manager, lazily loading any existing
// memories from the sidecar. Empty cacheDir/repoPath yields a no-disk
// manager. The sidecar lives at <cacheDir>/sidecar.sqlite; any legacy
// memories.gob.gz under the per-repo cache subdir is imported once,
// then renamed to *.bak.
func newMemoryManager(cacheDir, repoPath string) *memoryManager {
	if cacheDir == "" || repoPath == "" {
		return &memoryManager{}
	}
	sidecar, err := persistence.OpenSidecar(persistence.DefaultSidecarPath(cacheDir))
	if err != nil || sidecar == nil {
		return &memoryManager{}
	}
	return newMemoryManagerFromSidecar(sidecar, persistence.RepoCacheKey(repoPath), persistence.MemoriesDir(cacheDir, repoPath))
}

// newMemoryManagerFromSidecar builds a memory manager bound to an
// already-open sidecar + repo key, importing legacyDir/memories.gob.gz
// once. Used by the daemon path where the sidecar is opened once and
// shared across managers.
func newMemoryManagerFromSidecar(sidecar *persistence.SidecarStore, repoKey, legacyDir string) *memoryManager {
	mm := &memoryManager{repoKey: repoKey}
	if sidecar != nil {
		mm.sidecar = sidecar
		_ = sidecar.MigrateLegacyMemories(repoKey, legacyDir)
		if rows, err := sidecar.LoadMemoriesRows(repoKey); err == nil {
			mm.store.Entries = rows
		}
	}
	return mm
}

// allEntries returns a snapshot copy of every stored memory entry, for callers
// (e.g. the rationale projection) that need the full set under the lock.
func (m *memoryManager) allEntries() []persistence.MemoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]persistence.MemoryEntry, len(m.store.Entries))
	copy(out, m.store.Entries)
	return out
}

// MemoryQueryFilter constrains a Query call. Zero-value fields
// disable the corresponding filter; tag matching is exact
// (case-insensitive).
type MemoryQueryFilter struct {
	SymbolID    string
	FilePath    string
	Tag         string
	Kind        string
	Source      string
	AuthorAgent string
	TextSearch  string // case-insensitive substring against Body / Title
	Since       time.Time
	WorkspaceID string
	// WorkspaceIn is the set-valued form of WorkspaceID, for a session
	// bound to the repos its cwd contains: that shape spans several
	// workspaces and has no single slug, and an empty WorkspaceID means
	// "no filter", so without this such a session would read every
	// workspace's memories.
	WorkspaceIn       map[string]bool
	ProjectID         string
	Pinned            *bool
	MinImportance     int  // 0 = no filter; 1..5 = lower bound
	IncludeSuperseded bool // false (default) hides entries with SupersededBy != ""
	Limit             int
}

// MemoryPatch is the update payload — every pointer field is
// optional ("leave alone" when nil), every slice field replaces
// the prior value when non-nil. AddLinks is additive.
type MemoryPatch struct {
	Body         *string
	Title        *string
	Kind         *string
	Source       *string
	Confidence   *float32
	Importance   *int
	Pinned       *bool
	SupersededBy *string
	Tags         []string
	SymbolIDs    []string
	FilePaths    []string
	AddLinks     []string
}

// Save persists a new entry, returning the generated ID. Defaults
// for missing fields: Confidence=1.0, Importance=3, Kind="reference",
// Source="manual".
func (mm *memoryManager) Save(entry persistence.MemoryEntry) (string, error) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	if entry.ID == "" {
		entry.ID = newMemoryID()
	}
	now := time.Now().UTC()
	if entry.Timestamp.IsZero() {
		entry.Timestamp = now
	}
	entry.UpdatedAt = now

	entry.AutoLinks = dedupeStrings(entry.AutoLinks)
	entry.SymbolIDs = dedupeStrings(entry.SymbolIDs)
	entry.FilePaths = dedupeStrings(entry.FilePaths)
	entry.Tags = dedupeStrings(normaliseTags(entry.Tags))

	if entry.Confidence == 0 {
		entry.Confidence = 1.0
	}
	if entry.Importance == 0 {
		entry.Importance = 3
	}
	if entry.Kind == "" {
		entry.Kind = "reference"
	}
	if entry.Source == "" {
		entry.Source = "manual"
	}

	mm.store.Entries = append(mm.store.Entries, entry)
	if err := mm.persistLocked(entry); err != nil {
		return entry.ID, err
	}
	mm.trimLocked()
	return entry.ID, nil
}

// Update mutates an existing memory by ID. Pass nil for a field
// you don't want to change.
func (mm *memoryManager) Update(id string, patch MemoryPatch) (persistence.MemoryEntry, error) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	idx := mm.findLocked(id)
	if idx < 0 {
		return persistence.MemoryEntry{}, fmt.Errorf("memory %q not found", id)
	}
	e := mm.store.Entries[idx]
	if patch.Body != nil {
		e.Body = *patch.Body
	}
	if patch.Title != nil {
		e.Title = *patch.Title
	}
	if patch.Kind != nil {
		e.Kind = *patch.Kind
	}
	if patch.Source != nil {
		e.Source = *patch.Source
	}
	if patch.Confidence != nil {
		e.Confidence = *patch.Confidence
	}
	if patch.Importance != nil {
		e.Importance = *patch.Importance
	}
	if patch.Pinned != nil {
		e.Pinned = *patch.Pinned
	}
	if patch.SupersededBy != nil {
		e.SupersededBy = *patch.SupersededBy
	}
	if patch.Tags != nil {
		e.Tags = dedupeStrings(normaliseTags(patch.Tags))
	}
	if patch.SymbolIDs != nil {
		e.SymbolIDs = dedupeStrings(patch.SymbolIDs)
	}
	if patch.FilePaths != nil {
		e.FilePaths = dedupeStrings(patch.FilePaths)
	}
	if len(patch.AddLinks) > 0 {
		e.AutoLinks = dedupeStrings(append(append([]string{}, e.AutoLinks...), patch.AddLinks...))
	}
	e.UpdatedAt = time.Now().UTC()
	mm.store.Entries[idx] = e
	if err := mm.persistLocked(e); err != nil {
		return e, err
	}
	return e, nil
}

// Delete removes a memory by ID. Idempotent.
func (mm *memoryManager) Delete(id string) error {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	idx := mm.findLocked(id)
	if idx < 0 {
		return nil
	}
	mm.store.Entries = append(mm.store.Entries[:idx], mm.store.Entries[idx+1:]...)
	if mm.sidecar == nil {
		return nil
	}
	return mm.sidecar.DeleteMemory(mm.repoKey, id)
}

// Get returns a single memory by ID, or (zero, false) when not found.
func (mm *memoryManager) Get(id string) (persistence.MemoryEntry, bool) {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	idx := mm.findLocked(id)
	if idx < 0 {
		return persistence.MemoryEntry{}, false
	}
	return mm.store.Entries[idx], true
}

// MarkAccessed increments AccessCount and stamps LastAccessed on
// the given IDs. Best-effort: a flush failure is non-fatal because
// access stats are advisory.
func (mm *memoryManager) MarkAccessed(ids []string) {
	if len(ids) == 0 {
		return
	}
	mm.mu.Lock()
	defer mm.mu.Unlock()
	now := time.Now().UTC()
	for _, id := range ids {
		idx := mm.findLocked(id)
		if idx < 0 {
			continue
		}
		mm.store.Entries[idx].AccessCount++
		mm.store.Entries[idx].LastAccessed = now
		_ = mm.persistLocked(mm.store.Entries[idx])
	}
}

// Query returns memories matching every set filter. Results sort
// pinned-first, then by importance DESC, then by UpdatedAt DESC.
// Limit caps the slice after filtering & sorting.
func (mm *memoryManager) Query(f MemoryQueryFilter) []persistence.MemoryEntry {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	textNeedle := strings.ToLower(f.TextSearch)
	tagNeedle := strings.ToLower(f.Tag)

	var out []persistence.MemoryEntry
	for _, e := range mm.store.Entries {
		if !f.IncludeSuperseded && e.SupersededBy != "" {
			continue
		}
		if f.WorkspaceID != "" && e.WorkspaceID != f.WorkspaceID {
			continue
		}
		if len(f.WorkspaceIn) > 0 && !f.WorkspaceIn[e.WorkspaceID] {
			continue
		}
		if f.ProjectID != "" && e.ProjectID != f.ProjectID {
			continue
		}
		if f.Kind != "" && !strings.EqualFold(e.Kind, f.Kind) {
			continue
		}
		if f.Source != "" && !strings.EqualFold(e.Source, f.Source) {
			continue
		}
		if f.AuthorAgent != "" && !strings.EqualFold(e.AuthorAgent, f.AuthorAgent) {
			continue
		}
		if f.SymbolID != "" && !memoryReferencesSymbol(e, f.SymbolID) {
			continue
		}
		if f.FilePath != "" && !memoryReferencesFile(e, f.FilePath) {
			continue
		}
		if tagNeedle != "" && !hasTag(e.Tags, tagNeedle) {
			continue
		}
		if textNeedle != "" &&
			!strings.Contains(strings.ToLower(e.Body), textNeedle) &&
			!strings.Contains(strings.ToLower(e.Title), textNeedle) {
			continue
		}
		if !f.Since.IsZero() && e.UpdatedAt.Before(f.Since) {
			continue
		}
		if f.Pinned != nil && e.Pinned != *f.Pinned {
			continue
		}
		if f.MinImportance > 0 && e.Importance < f.MinImportance {
			continue
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].Importance != out[j].Importance {
			return out[i].Importance > out[j].Importance
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// HasData reports whether the store holds at least one memory.
func (mm *memoryManager) HasData() bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	return len(mm.store.Entries) > 0
}

// Count returns the total number of stored memories.
func (mm *memoryManager) Count() int {
	mm.mu.Lock()
	defer mm.mu.Unlock()
	return len(mm.store.Entries)
}

func (mm *memoryManager) findLocked(id string) int {
	for i := range mm.store.Entries {
		if mm.store.Entries[i].ID == id {
			return i
		}
	}
	return -1
}

// persistLocked writes a single memory row to the sidecar. No-op for
// an in-memory-only manager. Callers hold mm.mu.
func (mm *memoryManager) persistLocked(e persistence.MemoryEntry) error {
	if mm.sidecar == nil {
		return nil
	}
	return mm.sidecar.UpsertMemory(mm.repoKey, e)
}

// trimLocked enforces the soft cap (maxMemoriesCap) via the two-pass
// bounded DELETE on the sidecar, then reconciles the in-memory slice.
// No-op when under cap or in-memory-only. Callers hold mm.mu.
func (mm *memoryManager) trimLocked() {
	if mm.sidecar == nil || len(mm.store.Entries) <= maxMemoriesCap {
		return
	}
	if err := mm.sidecar.TrimMemories(mm.repoKey, maxMemoriesCap); err != nil {
		return
	}
	if rows, err := mm.sidecar.LoadMemoriesRows(mm.repoKey); err == nil {
		mm.store.Entries = rows
	}
}

// ---------------------------------------------------------------------------
// surface_memories — proactive retrieval given a working set
// ---------------------------------------------------------------------------

// SurfaceOptions tunes Surface. Defaults: Limit=10, ExcerptCap=320,
// MinScore=0, MarkAccessed=true, IncludeSuperseded=false.
type SurfaceOptions struct {
	Task        string
	SymbolIDs   []string
	FilePaths   []string
	WorkspaceID string
	// WorkspaceIn mirrors MemoryQueryFilter.WorkspaceIn — the set-valued
	// workspace filter a contained-repo session needs.
	WorkspaceIn       map[string]bool
	ProjectID         string
	Limit             int
	ExcerptCap        int
	MinScore          float32
	IncludeSuperseded bool
	MarkAccessed      bool
}

// surfaceResult is the structured digest returned by Surface. The
// shape is stable across JSON / TOON / GCX wire formats.
type surfaceResult struct {
	Task      string       `json:"task,omitempty"`
	Total     int          `json:"total"`
	Memories  []surfaceHit `json:"memories,omitempty"`
	Anchors   []string     `json:"anchors,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`

	// AnchorDiagnostic explains an empty result that anchors should have
	// filled. Surfacing works by string-equality between the caller's
	// symbol ids and the ids stored on each memory, so anything that
	// changes symbol-id SHAPE — a repo prefix appearing or disappearing,
	// a file move, a rename — silently severs every stored anchor and
	// returns Total: 0 that reads identically to "nothing relevant".
	// Populated only when there were anchors and no hits, so the happy
	// path pays nothing.
	AnchorDiagnostic string `json:"anchor_diagnostic,omitempty"`
	// StaleAnchors counts stored anchor symbol ids that no longer name a
	// live graph node. A high count against a healthy graph means the ids
	// were written under a different id convention.
	StaleAnchors       int      `json:"stale_anchors,omitempty"`
	StaleAnchorSamples []string `json:"stale_anchor_samples,omitempty"`
}

type surfaceHit struct {
	ID           string    `json:"id"`
	Title        string    `json:"title,omitempty"`
	Body         string    `json:"body"`
	Kind         string    `json:"kind,omitempty"`
	Source       string    `json:"source,omitempty"`
	Tags         []string  `json:"tags,omitempty"`
	SymbolIDs    []string  `json:"symbol_ids,omitempty"`
	FilePaths    []string  `json:"file_paths,omitempty"`
	Importance   int       `json:"importance,omitempty"`
	Confidence   float32   `json:"confidence,omitempty"`
	Pinned       bool      `json:"pinned,omitempty"`
	Score        float32   `json:"score"`
	UpdatedAt    time.Time `json:"updated_at"`
	MatchReasons []string  `json:"match_reasons,omitempty"`
}

// diagnoseEmptyAnchoredResult explains a surface that returned nothing
// despite the caller supplying anchors and the store holding memories.
//
// The common cause is not "nothing relevant" but a symbol-id SHAPE change:
// surfacing matches stored ids against caller ids by string equality, so a
// repo prefix appearing (or disappearing) severs every stored anchor at
// once. Counting the stored anchors that no longer name a live graph node
// separates that from a genuine miss — a store full of ids the graph has
// never heard of is a convention mismatch, not an empty topic.
//
// Bounded: at most staleAnchorProbeCap ids are probed, so a large store
// cannot turn an empty result into an expensive one.
func (res *surfaceResult) diagnoseEmptyAnchoredResult(
	candidates []persistence.MemoryEntry,
	resolveNode func(string) *graph.Node,
) {
	if resolveNode == nil {
		return
	}
	const staleAnchorProbeCap = 200
	const staleAnchorSampleCap = 3

	probed, live := 0, 0
	seen := make(map[string]struct{}, staleAnchorProbeCap)
	for _, e := range candidates {
		for _, sym := range e.SymbolIDs {
			if sym == "" {
				continue
			}
			if _, dup := seen[sym]; dup {
				continue
			}
			seen[sym] = struct{}{}
			if probed >= staleAnchorProbeCap {
				break
			}
			probed++
			if resolveNode(sym) != nil {
				live++
				continue
			}
			res.StaleAnchors++
			if len(res.StaleAnchorSamples) < staleAnchorSampleCap {
				res.StaleAnchorSamples = append(res.StaleAnchorSamples, sym)
			}
		}
	}
	if probed == 0 {
		return
	}
	// Most stored anchors dangling is a convention mismatch. A few are
	// ordinary drift (deleted or renamed symbols) and not worth alarming
	// about, so the message differs by degree.
	if res.StaleAnchors*2 > probed {
		res.AnchorDiagnostic = "no memories matched, and most stored anchor symbol ids do not name a live graph node — " +
			"these memories were written against a different symbol-id convention (e.g. before repo prefixes were added to node ids), " +
			"so anchor matching cannot succeed until they are re-anchored"
		return
	}
	if res.StaleAnchors > 0 {
		res.AnchorDiagnostic = "no memories matched; some stored anchors no longer name a live graph node (renamed or deleted symbols)"
	}
}

// Surface returns memories ranked by relevance to a set of anchor
// symbols / files plus an optional task description.
//
// Scoring (deterministic, no LLM):
//
//	+3.0  per anchor symbol overlap with SymbolIDs
//	+1.5  per anchor symbol overlap with AutoLinks
//	+1.5  per anchor file overlap with FilePaths
//	+1.0  per task-keyword hit in body or title
//	+0.5  × importance
//	+0.4  if pinned
//	+0.3  if updated within the last 30 days
//	+0.2  if AccessCount > 0 (already proven useful)
//	*= confidence (when 0 < confidence < 1)
//
// Containing files of anchor symbols are added to the file set
// automatically — a memory anchored to the file usually applies
// when you're editing any symbol inside it.
//
// When at least one anchor / keyword is provided, a memory must
// match at least one reason (or be pinned) to be returned. When
// no anchors are provided, every memory is returned, ranked by
// importance + recency.
func (mm *memoryManager) Surface(opts SurfaceOptions, resolveNode func(string) *graph.Node) surfaceResult {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}

	res := surfaceResult{Task: opts.Task}

	symbolSet := make(map[string]struct{}, len(opts.SymbolIDs))
	for _, s := range opts.SymbolIDs {
		if s != "" {
			symbolSet[s] = struct{}{}
		}
	}
	fileSet := make(map[string]struct{}, len(opts.FilePaths))
	for _, f := range opts.FilePaths {
		if f != "" {
			fileSet[f] = struct{}{}
		}
	}

	// Promote anchor symbols → their containing files (free signal).
	if resolveNode != nil {
		for sym := range symbolSet {
			if node := resolveNode(sym); node != nil && node.FilePath != "" {
				fileSet[node.FilePath] = struct{}{}
			}
		}
	}

	for s := range symbolSet {
		res.Anchors = append(res.Anchors, s)
	}
	sort.Strings(res.Anchors)

	keywords := tokeniseIdentifiers(opts.Task, 3)
	keywordSet := make(map[string]struct{}, len(keywords))
	for _, k := range keywords {
		keywordSet[strings.ToLower(k)] = struct{}{}
	}

	hasAnchor := len(symbolSet)+len(fileSet)+len(keywordSet) > 0

	mm.mu.Lock()
	candidates := make([]persistence.MemoryEntry, 0, len(mm.store.Entries))
	for _, e := range mm.store.Entries {
		if e.SupersededBy != "" && !opts.IncludeSuperseded {
			continue
		}
		if opts.WorkspaceID != "" && e.WorkspaceID != opts.WorkspaceID {
			continue
		}
		if len(opts.WorkspaceIn) > 0 && !opts.WorkspaceIn[e.WorkspaceID] {
			continue
		}
		if opts.ProjectID != "" && e.ProjectID != opts.ProjectID {
			continue
		}
		candidates = append(candidates, e)
	}
	mm.mu.Unlock()

	now := time.Now().UTC()
	scored := make([]surfaceHit, 0, len(candidates))
	for _, e := range candidates {
		var score float32
		var reasons []string

		for _, s := range e.SymbolIDs {
			if _, ok := symbolSet[s]; ok {
				score += 3.0
				reasons = append(reasons, "symbol:"+s)
			}
		}
		for _, s := range e.AutoLinks {
			if _, ok := symbolSet[s]; ok {
				score += 1.5
				reasons = append(reasons, "link:"+s)
			}
		}
		for _, f := range e.FilePaths {
			if _, ok := fileSet[f]; ok {
				score += 1.5
				reasons = append(reasons, "file:"+f)
			}
		}
		if len(keywordSet) > 0 {
			bodyLower := strings.ToLower(e.Body)
			titleLower := strings.ToLower(e.Title)
			for k := range keywordSet {
				if strings.Contains(bodyLower, k) || strings.Contains(titleLower, k) {
					score += 1.0
					reasons = append(reasons, "kw:"+k)
				}
			}
		}
		if e.Importance > 0 {
			score += float32(e.Importance) * 0.5
		}
		if e.Pinned {
			score += 0.4
			reasons = append(reasons, "pinned")
		}
		if !e.UpdatedAt.IsZero() && now.Sub(e.UpdatedAt) < 30*24*time.Hour {
			score += 0.3
		}
		if e.AccessCount > 0 {
			score += 0.2
		}
		if e.Confidence > 0 && e.Confidence < 1 {
			score *= e.Confidence
		}

		if score < opts.MinScore {
			continue
		}
		// With anchors/keywords: require at least one match reason
		// (pinned counts). Without anchors: return everything.
		if hasAnchor && len(reasons) == 0 {
			continue
		}

		body := e.Body
		if opts.ExcerptCap > 0 && len(body) > opts.ExcerptCap {
			body = body[:opts.ExcerptCap] + "…"
		}

		scored = append(scored, surfaceHit{
			ID:           e.ID,
			Title:        e.Title,
			Body:         body,
			Kind:         e.Kind,
			Source:       e.Source,
			Tags:         append([]string{}, e.Tags...),
			SymbolIDs:    append([]string{}, e.SymbolIDs...),
			FilePaths:    append([]string{}, e.FilePaths...),
			Importance:   e.Importance,
			Confidence:   e.Confidence,
			Pinned:       e.Pinned,
			Score:        score,
			UpdatedAt:    e.UpdatedAt,
			MatchReasons: reasons,
		})
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		if scored[i].Pinned != scored[j].Pinned {
			return scored[i].Pinned
		}
		return scored[i].UpdatedAt.After(scored[j].UpdatedAt)
	})

	res.Total = len(scored)
	if len(scored) > opts.Limit {
		scored = scored[:opts.Limit]
		res.Truncated = true
	}
	res.Memories = scored

	// Anchors supplied, memories present, nothing matched: explain it
	// rather than returning a bare zero. Only runs on the empty path, so
	// the resolveNode calls below cost nothing in the common case.
	if len(res.Memories) == 0 && hasAnchor && len(candidates) > 0 {
		res.diagnoseEmptyAnchoredResult(candidates, resolveNode)
	}

	if opts.MarkAccessed && len(res.Memories) > 0 {
		ids := make([]string, 0, len(res.Memories))
		for _, h := range res.Memories {
			ids = append(ids, h.ID)
		}
		mm.MarkAccessed(ids)
	}

	return res
}

// memoryReferencesSymbol reports whether the memory is attached to
// the given symbol either directly (SymbolIDs) or via auto-link.
func memoryReferencesSymbol(e persistence.MemoryEntry, sym string) bool {
	if slices.Contains(e.SymbolIDs, sym) {
		return true
	}
	return slices.Contains(e.AutoLinks, sym)
}

// memoryReferencesFile reports whether the memory is attached to
// the given file via FilePaths.
func memoryReferencesFile(e persistence.MemoryEntry, path string) bool {
	return slices.Contains(e.FilePaths, path)
}

// newMemoryID returns an 8-byte hex token. Crypto-strength because
// the IDs travel through MCP responses and can be referenced by
// other tool calls — predictability would let one session guess
// another's memory IDs on a shared cache.
func newMemoryID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("mem%016x", time.Now().UnixNano())
	}
	return "mem" + hex.EncodeToString(b[:])
}
