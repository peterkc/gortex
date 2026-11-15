package query

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

type pathBundleBackend struct {
	scopedBundleBackend
	pathCalls    int
	paths, repos []string
	pathLimit    int
	answer       []search.SymbolBundle
	handled      bool
	failure      *search.SymbolSearcherBackend
}

func (b *pathBundleBackend) SearchSymbolBundlesPathScopedContext(ctx context.Context, q string, repos, paths []string, limit int) ([]search.SymbolBundle, bool) {
	b.pathCalls++
	b.paths = paths
	b.repos = repos
	b.pathLimit = limit
	if b.failure != nil {
		return b.failure.SearchSymbolBundlesPathScopedContext(ctx, q, repos, paths, limit)
	}
	return b.answer, b.handled
}

func TestGatherPathBundleScopeIsAuthoritativeAndFallbackIsConservative(t *testing.T) {
	for _, mode := range []string{"match", "zero", "failure", "unsupported", "composed", "overlay"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			node := &graph.Node{ID: "repo/target/a.go::Needle", Name: "needle", Kind: graph.KindFunction, RepoPrefix: "repo", FilePath: "repo/target/a.go"}
			g.AddNode(node)
			b := &pathBundleBackend{handled: true, answer: []search.SymbolBundle{}, scopedBundleBackend: scopedBundleBackend{flood: []search.SymbolBundle{{Node: node}}, scoped: []search.SymbolBundle{{Node: node}}}}
			eng := NewEngine(g)
			eng.SetSearch(b)
			eng.SetRerank(nil)
			opts := QueryOptions{RepoAllow: map[string]bool{"repo": true}, SearchPathPrefixes: []string{"target"}, SkipVectorChannel: true, SkipInnerRerank: true, SymbolSearchStats: &SymbolSearchStats{}, SearchNodeFilter: func(n *graph.Node) bool { return n.FilePath == "repo/target/a.go" }}
			var failureErr error
			switch mode {
			case "match":
				b.answer = []search.SymbolBundle{{Node: node}}
			case "failure":
				b.answer = nil
				failureErr = errors.New("path operation failed")
				b.failure = search.NewSymbolSearcherBackend(&requestFailureSymbolStore{err: failureErr})
			case "unsupported":
				b.handled = false
			case "composed":
				eng.viewLayers = []ViewLayerSource{{}}
			case "overlay":
				eng.overlay = &graph.OverlayLayer{}
			}
			var failures []string
			ctx := search.WithSearchFailureObserver(context.Background(), func(channel string, err error) {
				if errors.Is(err, failureErr) {
					failures = append(failures, channel)
				}
			})
			got := eng.GatherSymbolCandidatesContext(ctx, "needle", 1, opts, nil)
			if mode == "failure" {
				// A failed text operation must not replay, but independent in-scope exact evidence survives.
				if len(got) != 1 || got[0].Node.ID != node.ID || !reflect.DeepEqual(failures, []string{"text"}) || b.scopedCalls != 0 || b.unscopedCalls != 0 || b.searchCalls != 0 {
					t.Fatalf("failed text lost exact evidence, warning or replayed: got%v failures%v backend%+v", got, failures, b)
				}
			} else if len(got) != 1 || got[0].Node.ID != node.ID { // authoritative zero can still use in-scope exact/substring supplements
				t.Fatalf("mode%s results%v", mode, got)
			}
			wantCalls := 1
			if mode == "composed" || mode == "overlay" {
				wantCalls = 0
			}
			if b.pathCalls != wantCalls {
				t.Fatalf("pathcalls=%d want%d", b.pathCalls, wantCalls)
			}
			if wantCalls == 1 && (!reflect.DeepEqual(b.paths, opts.SearchPathPrefixes) || !reflect.DeepEqual(b.repos, []string{"repo"}) || b.pathLimit < 1) {
				t.Fatalf("forwarded scope%+v", b)
			}
			if mode == "zero" && (b.scopedCalls != 0 || b.unscopedCalls != 0 || b.searchCalls != 0 || opts.SymbolSearchStats.TextSaturated) {
				t.Fatalf("zero flooded/refetched: %+v", b)
			}
		})
	}
}
