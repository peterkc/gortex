// Package adapter is the stable contract implemented by each study binary.
package adapter

import (
	"context"
	"errors"
	"gxstudy/dataset"
	"slices"
)

var ErrNotFound = errors.New("not found")
var ErrUnsupported = errors.New("unsupported operation")

var ErrConflict = errors.New("catalog compare-and-set conflict")

type RankPolicy struct {
	// Mode is "harness" (also the omitted default) or "native" lexical scoring.
	Mode       string  `json:"mode,omitempty"`
	Exact      bool    `json:"exact"`
	MinOverlap float64 `json:"min_overlap"`
}
type Capabilities struct {
	// Keys are workload operation names (id, name, in, content, multi_vector...).
	// A reason declares the operation unsupported; supported errors still fail.
	Unsupported map[string]string     `json:"unsupported,omitempty"`
	Ranking     map[string]RankPolicy `json:"ranking,omitempty"`

	Operations map[string]string `json:"operations"`
	Libraries  []string          `json:"libraries"`
	Deployment string            `json:"deployment"`
	Notes      []string          `json:"notes"`
}
type SegmentRef struct {
	Repo    string `json:"repo"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Digest  string `json:"digest"`
}
type BuildStats struct {
	Mode  string   `json:"mode"`
	Bytes int64    `json:"bytes"`
	Notes []string `json:"notes,omitempty"`
}
type FileChange struct {
	Path        string
	Replacement dataset.Dataset
}
type Hit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}
type Backend interface {
	Name() string
	Capabilities() Capabilities
	Build(context.Context, dataset.Dataset, string) (SegmentRef, BuildStats, error)
	Open(context.Context, string, SegmentRef) (Segment, error)
	BuildDelta(context.Context, SegmentRef, FileChange, string) (SegmentRef, BuildStats, error)
}

// Segment is immutable and concurrent-reader safe. NodeByID alone returns
// ErrNotFound for absence. All list operations return an empty list and nil error
// for unknown keys. NodesByName sorts by ID before applying limit (0 empty,
// negative unlimited). File/edge order is unspecified; duplicates are retained.
// All payload fields must round-trip. Hits are unique, finite, descending-score,
// ID-tied results of at most k entries; a missing search returns empty,nil.
type Segment interface {
	NodeByID(context.Context, string) (dataset.Node, error)
	NodesByName(context.Context, string, int) ([]dataset.Node, error)
	FileNodes(context.Context, string) ([]dataset.Node, error)
	OutEdges(context.Context, string, string) ([]dataset.Edge, error)
	InEdges(context.Context, string, string) ([]dataset.Edge, error)
	SearchSymbols(context.Context, string, int) ([]Hit, error)
	SearchContent(context.Context, string, int) ([]Hit, error)
	VectorTopK(context.Context, []float32, int) ([]Hit, error)
	Close() error
}
type MultiRepo interface {
	NodesByName(context.Context, string, int) ([]dataset.Node, error)
	InEdges(context.Context, string, string) ([]dataset.Edge, error)
	SearchSymbols(context.Context, string, int) ([]Hit, error)
	VectorTopK(context.Context, []float32, int) ([]Hit, error)
}

// NativeMulti is optional. The runner always measures default fan-out as well.
type NativeMulti interface {
	OpenMulti(context.Context, map[string]Segment) (MultiRepo, error)
}

// NativeTraverser is optional on Segment or MultiRepo. Semantics match the
// harness BFS: sorted unique caller IDs, per-node cap, breadth-first visit cap,
// exclude the start, and return every visited ID through depth, sorted by ID.
type NativeTraverser interface {
	NativeTraverse(context.Context, string, int, int, int) ([]string, error)
}

// ServerHooks is optional. Reset runs before and after each fresh-process run.
// Release runs for each built ref after all pins/segments close. Both are outside
// timing. ResourceUsage is sampled outside timing after each named phase.
type ResourceUsage struct {
	RSSBytes  int64    `json:"rss_bytes"`
	DiskBytes int64    `json:"disk_bytes"`
	Notes     []string `json:"notes,omitempty"`
}
type ServerHooks interface {
	Reset(context.Context) error
	Release(context.Context, SegmentRef) error
	ResourceUsage(context.Context) (ResourceUsage, error)
}

type View map[string]SegmentRef

// Catalog CAS compares the complete SegmentRef. Publish(old=nil) requires
// absence. In PublishSet, a missing expected-old key requires that repo absent;
// expected keys outside Set are ignored. A failed CAS changes nothing. The set
// commits atomically. Pin returns one coherent snapshot and retains its complete
// versions until its idempotent release runs. Unknown Pin/Previous => ErrNotFound.
// Recover is idempotent. CommitPoint describes the actual atomic commit effect.
// before-rename means before that effect; after-rename means after that effect.
type Catalog interface {
	CommitPoint() string

	Publish(context.Context, string, SegmentRef, *SegmentRef) error
	PublishSet(context.Context, View, View) error
	Pin(context.Context, []string) (View, func(), error)
	Previous(context.Context, string) (SegmentRef, error)
	Recover(context.Context) error
	Close() error
}
type CatalogFactory func(dir string) (Catalog, error)

// Checkpoint is called by catalog implementations at publication boundaries.
// Candidates call this same hook to support reproducible child-process SIGKILL.
var Checkpoint = func(stage string) {}

type SearchInput struct {
	Repo string
	Hits []Hit
}
type Fanout struct {
	Segments map[string]Segment
	// Optional preallocated trace. Its slice headers are filled during search;
	// the runner validates the inputs and own-score merge outside the timer.
	SymbolInputs []SearchInput
}

func (f Fanout) ordered() []string {
	a := make([]string, 0, len(f.Segments))
	for id := range f.Segments {
		a = append(a, id)
	}
	slices.Sort(a)
	return a
}
func (f Fanout) NodesByName(ctx context.Context, name string, k int) ([]dataset.Node, error) {
	var a []dataset.Node
	for _, id := range f.ordered() {
		v, e := f.Segments[id].NodesByName(ctx, name, k)
		if e != nil {
			return nil, e
		}
		a = append(a, v...)
	}
	slices.SortFunc(a, func(a, b dataset.Node) int { return cmpString(a.ID, b.ID) })
	if k >= 0 && len(a) > k {
		a = a[:k]
	}
	return a, nil
}
func (f Fanout) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	var a []dataset.Edge
	for _, r := range f.ordered() {
		v, e := f.Segments[r].InEdges(ctx, id, kind)
		if e != nil {
			return nil, e
		}
		a = append(a, v...)
	}
	return a, nil
}
func (f Fanout) SearchSymbols(ctx context.Context, q string, k int) ([]Hit, error) {
	var a []Hit
	for i, r := range f.ordered() {
		v, e := f.Segments[r].SearchSymbols(ctx, q, k)
		if e != nil {
			return nil, e
		}
		if len(f.SymbolInputs) == len(f.Segments) {
			f.SymbolInputs[i] = SearchInput{Repo: r, Hits: v}
		}
		a = append(a, v...)
	}
	return Top(a, k), nil
}
func (f Fanout) VectorTopK(ctx context.Context, v []float32, k int) ([]Hit, error) {
	var a []Hit
	for _, r := range f.ordered() {
		h, e := f.Segments[r].VectorTopK(ctx, v, k)
		if e != nil {
			return nil, e
		}
		a = append(a, h...)
	}
	return Top(a, k), nil
}
func cmpString(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
func Top(a []Hit, k int) []Hit {
	a = slices.Clone(a)
	slices.SortFunc(a, func(a, b Hit) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return cmpString(a.ID, b.ID)
	})
	if k >= 0 && len(a) > k {
		a = a[:k]
	}
	return a
}

// ApplyChange replaces records owned by one source file. Incoming edges in other
// files survive, including edges that become dangling after a symbol deletion.
func ApplyChange(base dataset.Dataset, c FileChange) dataset.Dataset {
	d := base
	d.Nodes = nil
	d.Edges = nil
	d.Files = nil
	d.Symbols = nil
	d.Content = nil
	d.Vectors = nil
	removed := map[string]bool{}
	for _, n := range base.Nodes {
		if n.FilePath == c.Path {
			removed[n.ID] = true
		} else {
			d.Nodes = append(d.Nodes, n)
		}
	}
	for _, e := range base.Edges {
		if e.FilePath != c.Path && !removed[e.From] {
			d.Edges = append(d.Edges, e)
		}
	}
	for _, f := range base.Files {
		if f.Path != c.Path {
			d.Files = append(d.Files, f)
		}
	}
	for _, s := range base.Symbols {
		if !removed[s.NodeID] {
			d.Symbols = append(d.Symbols, s)
		}
	}
	for _, s := range base.Content {
		if s.FilePath != c.Path && !removed[s.NodeID] {
			d.Content = append(d.Content, s)
		}
	}
	for _, v := range base.Vectors {
		if !removed[v.NodeID] {
			d.Vectors = append(d.Vectors, v)
		}
	}
	r := c.Replacement
	d.Nodes = append(d.Nodes, r.Nodes...)
	d.Edges = append(d.Edges, r.Edges...)
	d.Files = append(d.Files, r.Files...)
	d.Symbols = append(d.Symbols, r.Symbols...)
	d.Content = append(d.Content, r.Content...)
	d.Vectors = append(d.Vectors, r.Vectors...)
	return d
}

// PublicationVerifier prepares a complete ref outside the commit timing window.
// The reference catalog caches successful validation and does not reopen it in
// Publish. A catalog without this interface is verified with Backend.Open first.
type PublicationVerifier interface {
	VerifyPublication(context.Context, SegmentRef) error
}
