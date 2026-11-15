package mcp

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type exploreImplementationAdjacencyRequest struct {
	ids   []string
	kinds []graph.EdgeKind
	limit int
}

type exploreImplementationFaultReader struct {
	graph.Reader
	outgoing func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error)
	incoming func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error)
	nodes    func(context.Context, []string) (map[string]*graph.Node, error)

	outgoingRequests []exploreImplementationAdjacencyRequest
	incomingRequests []exploreImplementationAdjacencyRequest
	nodeRequests     [][]string
}

func (r *exploreImplementationFaultReader) GetOutEdges(string) []*graph.Edge {
	panic("legacy GetOutEdges must not be called")
}

func (r *exploreImplementationFaultReader) GetInEdges(string) []*graph.Edge {
	panic("legacy GetInEdges must not be called")
}

func (r *exploreImplementationFaultReader) FindOutgoingEdgeIdentitiesBounded(
	ctx context.Context,
	ids []string,
	kinds []graph.EdgeKind,
	limit int,
) (graph.BoundedEdgeIdentityProjection, error) {
	r.outgoingRequests = append(r.outgoingRequests, exploreImplementationAdjacencyRequest{
		ids: append([]string(nil), ids...), kinds: append([]graph.EdgeKind(nil), kinds...), limit: limit,
	})
	if r.outgoing != nil {
		return r.outgoing(ctx, ids, kinds, limit)
	}
	return r.Reader.(graph.BoundedOutgoingEdgeIdentityReader).FindOutgoingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
}

func (r *exploreImplementationFaultReader) FindIncomingEdgeIdentitiesBounded(
	ctx context.Context,
	ids []string,
	kinds []graph.EdgeKind,
	limit int,
) (graph.BoundedEdgeIdentityProjection, error) {
	r.incomingRequests = append(r.incomingRequests, exploreImplementationAdjacencyRequest{
		ids: append([]string(nil), ids...), kinds: append([]graph.EdgeKind(nil), kinds...), limit: limit,
	})
	if r.incoming != nil {
		return r.incoming(ctx, ids, kinds, limit)
	}
	return r.Reader.(graph.BoundedIncomingEdgeIdentityReader).FindIncomingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
}

func (r *exploreImplementationFaultReader) GetNodesByIDsContext(
	ctx context.Context,
	ids []string,
) (map[string]*graph.Node, error) {
	r.nodeRequests = append(r.nodeRequests, append([]string(nil), ids...))
	if r.nodes != nil {
		return r.nodes(ctx, append([]string(nil), ids...))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.GetNodesByIDs(ids), nil
}

type exploreImplementationLegacyOnlyReader struct{ graph.Reader }

func (r *exploreImplementationLegacyOnlyReader) GetOutEdges(string) []*graph.Edge {
	panic("legacy GetOutEdges must not be called")
}

func (r *exploreImplementationLegacyOnlyReader) GetInEdges(string) []*graph.Edge {
	panic("legacy GetInEdges must not be called")
}

type exploreImplementationCancelAfterChecksContext struct {
	context.Context
	remaining int
}

func (ctx *exploreImplementationCancelAfterChecksContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func exploreImplementationTestServer(reader graph.Reader) *Server {
	return &Server{engine: query.NewEngine(graph.New()).WithReader(reader)}
}

func TestImplementationRecoveryUsesExactBoundedCapabilities(t *testing.T) {
	_, base := newStorageFixtureServer(t)
	reader := &exploreImplementationFaultReader{Reader: base}
	server := exploreImplementationTestServer(reader)
	seed := exploreTarget{node: base.GetNode("repo/storage/storage.go::Storage.load"), score: 1}
	if got := server.expandImplementationTargets(context.Background(), []exploreTarget{seed}, graph.LocalizationNodeScope{}); len(got) < 2 {
		t.Fatalf("bounded implementation recovery returned %d targets", len(got))
	}
	if len(reader.outgoingRequests) != 1 || reader.outgoingRequests[0].limit != exploreImplOwnerRelationLimit ||
		len(reader.outgoingRequests[0].kinds) != 1 || reader.outgoingRequests[0].kinds[0] != graph.EdgeMemberOf {
		t.Fatalf("owner request = %#v", reader.outgoingRequests)
	}
	foundImplementors, foundMembers := false, false
	for _, request := range reader.incomingRequests {
		switch request.limit {
		case exploreImplImplementorLimit:
			foundImplementors = len(request.kinds) == 2 && request.kinds[0] == graph.EdgeImplements && request.kinds[1] == graph.EdgeExtends
		case exploreImplMemberRefinementLimit:
			foundMembers = len(request.kinds) == 1 && request.kinds[0] == graph.EdgeMemberOf
		}
	}
	if !foundImplementors || !foundMembers {
		t.Fatalf("incoming requests = %#v", reader.incomingRequests)
	}
	wantNodeRequests := [][]string{
		{"repo/storage/storage.go::Storage"},
		{"repo/storage/cloud.go::CloudStorage", "repo/storage/disk.go::DiskStorage"},
		{"repo/storage/cloud.go::CloudStorage.load"},
		{"repo/storage/disk.go::DiskStorage.load"},
	}
	if len(reader.nodeRequests) != len(wantNodeRequests) {
		t.Fatalf("node requests = %#v, want %#v", reader.nodeRequests, wantNodeRequests)
	}
	for index := range wantNodeRequests {
		if len(reader.nodeRequests[index]) != len(wantNodeRequests[index]) {
			t.Fatalf("node request %d = %#v, want %#v", index, reader.nodeRequests[index], wantNodeRequests[index])
		}
		for item := range wantNodeRequests[index] {
			if reader.nodeRequests[index][item] != wantNodeRequests[index][item] {
				t.Fatalf("node request %d = %#v, want %#v", index, reader.nodeRequests[index], wantNodeRequests[index])
			}
		}
	}

	foldServer, foldBase := newOwnerFoldFixture(t)
	_ = foldServer
	foldReader := &exploreImplementationFaultReader{Reader: foldBase}
	members := []exploreTarget{
		{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.serialize"), score: 1},
		{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.hydrate"), score: .9},
	}
	if got := exploreImplementationTestServer(foldReader).foldMemberOwners(context.Background(), members, graph.LocalizationNodeScope{}); len(got) != 3 {
		t.Fatalf("bounded owner fold returned %d targets", len(got))
	}
	if len(foldReader.outgoingRequests) != 1 || foldReader.outgoingRequests[0].limit != exploreOwnerFoldRelationLimit ||
		len(foldReader.outgoingRequests[0].ids) != 2 {
		t.Fatalf("fold request = %#v", foldReader.outgoingRequests)
	}
	if len(foldReader.nodeRequests) != 1 || len(foldReader.nodeRequests[0]) != 1 ||
		foldReader.nodeRequests[0][0] != "repo/mw/persist.ts::PersistOptions" {
		t.Fatalf("fold node requests = %#v", foldReader.nodeRequests)
	}
}

func TestImplementationRecoveryHasNoLegacyAdjacencyFallback(t *testing.T) {
	_, base := newStorageFixtureServer(t)
	reader := &exploreImplementationLegacyOnlyReader{Reader: base}
	server := exploreImplementationTestServer(reader)
	method := exploreTarget{node: base.GetNode("repo/storage/storage.go::Storage.load"), score: 1}
	// Given unavailable optional adjacency, retain every field of the ordinary ranked evidence.
	if got := server.expandImplementationTargets(context.Background(), []exploreTarget{method}, graph.LocalizationNodeScope{}); !reflect.DeepEqual(got, []exploreTarget{method}) {
		t.Fatalf("unsupported expansion changed ranked evidence: %#v", got)
	}
	if !exploreImplementationAnswerBlocked(
		context.Background(), "find the concrete implementation", []exploreTarget{method}, reader, graph.LocalizationNodeScope{},
	) {
		t.Fatal("unsupported terminality lookup must conservatively block")
	}

	_, foldBase := newOwnerFoldFixture(t)
	foldReader := &exploreImplementationLegacyOnlyReader{Reader: foldBase}
	members := []exploreTarget{
		{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.serialize")},
		{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.hydrate")},
	}
	if got := exploreImplementationTestServer(foldReader).foldMemberOwners(context.Background(), members, graph.LocalizationNodeScope{}); !reflect.DeepEqual(got, members) {
		t.Fatalf("unsupported owner fold changed ranked evidence: %#v", got)
	}
}

func TestImplementationRecoveryDiscardsFaultedOrCanceledProjections(t *testing.T) {
	_, base := newStorageFixtureServer(t)
	method := exploreTarget{node: base.GetNode("repo/storage/storage.go::Storage.load"), score: 1}
	partialOwner := graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{
		method.node.ID: {{From: method.node.ID, To: "repo/storage/storage.go::Storage", Kind: graph.EdgeMemberOf}},
	}}

	t.Run("partial error", func(t *testing.T) {
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.outgoing = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			return partialOwner, errors.New("projection failed")
		}
		server := exploreImplementationTestServer(reader)
		if got := server.expandImplementationTargets(context.Background(), []exploreTarget{method}, graph.LocalizationNodeScope{}); len(got) != 1 {
			t.Fatalf("partial-error expansion returned %d targets", len(got))
		}
		if !exploreImplementationAnswerBlocked(context.Background(), "find implementation", []exploreTarget{method}, reader, graph.LocalizationNodeScope{}) {
			t.Fatal("partial-error terminality lookup must block")
		}
	})

	t.Run("sticky cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.outgoing = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			cancel()
			return partialOwner, nil
		}
		if !exploreImplementationAnswerBlocked(ctx, "find implementation", []exploreTarget{method}, reader, graph.LocalizationNodeScope{}) {
			t.Fatal("sticky cancellation must block terminality")
		}
	})

	t.Run("missing hydration", func(t *testing.T) {
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.nodes = func(context.Context, []string) (map[string]*graph.Node, error) {
			return map[string]*graph.Node{}, nil
		}
		server := exploreImplementationTestServer(reader)
		if got := server.expandImplementationTargets(context.Background(), []exploreTarget{method}, graph.LocalizationNodeScope{}); len(got) != 1 {
			t.Fatalf("missing-hydration expansion returned %d targets", len(got))
		}
	})

	t.Run("malformed endpoint", func(t *testing.T) {
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.outgoing = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			return graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{
				method.node.ID: {{From: "foreign", To: "repo/storage/storage.go::Storage", Kind: graph.EdgeMemberOf}},
			}}, nil
		}
		if !exploreImplementationAnswerBlocked(context.Background(), "find implementation", []exploreTarget{method}, reader, graph.LocalizationNodeScope{}) {
			t.Fatal("malformed endpoint must block terminality")
		}
	})
}

func TestImplementationAnswerBlockedChecksCancellationAfterClassification(t *testing.T) {
	base := graph.New()
	concrete := &graph.Node{ID: "repo/impl.go::Concrete", Kind: graph.KindType, Name: "Concrete", FilePath: "repo/impl.go"}
	base.AddNode(concrete)
	reader := &exploreImplementationFaultReader{Reader: base}
	ctx := &exploreImplementationCancelAfterChecksContext{Context: context.Background(), remaining: 2}
	if !exploreImplementationAnswerBlocked(
		ctx, "find implementation", []exploreTarget{{node: concrete}}, reader, graph.LocalizationNodeScope{},
	) {
		t.Fatal("cancellation after concrete classification must block terminality")
	}
}

func TestExploreBoundedEdgeIdentitiesRejectsMalformedProjection(t *testing.T) {
	endpoint := "source"
	valid := graph.EdgeIdentity{From: endpoint, To: "owner", Kind: graph.EdgeMemberOf}
	for _, test := range []struct {
		name       string
		limit      int
		projection graph.BoundedEdgeIdentityProjection
	}{
		{name: "foreign key", projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{"foreign": {valid}}}},
		{name: "wrong kind", projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{endpoint: {{From: endpoint, To: "owner", Kind: graph.EdgeReads}}}}},
		{name: "wrong source", projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{endpoint: {{From: "foreign", To: "owner", Kind: graph.EdgeMemberOf}}}}},
		{name: "empty target", projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{endpoint: {{From: endpoint, Kind: graph.EdgeMemberOf}}}}},
		{name: "cardinality", projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{endpoint: {valid, {From: endpoint, To: "owner-2", Kind: graph.EdgeMemberOf}}}}},
		{name: "duplicate within limit", limit: 2, projection: graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{endpoint: {valid, valid}}}},
		{name: "truncated", projection: graph.BoundedEdgeIdentityProjection{Truncated: map[string]bool{endpoint: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &exploreImplementationFaultReader{Reader: graph.New()}
			reader.outgoing = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
				return test.projection, nil
			}
			limit := test.limit
			if limit == 0 {
				limit = 1
			}
			if rows, ok := exploreBoundedEdgeIdentities(
				context.Background(), reader, []string{endpoint}, []graph.EdgeKind{graph.EdgeMemberOf}, limit, exploreBoundedOutgoing,
			); ok || rows != nil {
				t.Fatalf("malformed projection accepted: ok=%v rows=%v", ok, rows)
			}
		})
	}

	t.Run("incoming wrong target", func(t *testing.T) {
		reader := &exploreImplementationFaultReader{Reader: graph.New()}
		reader.incoming = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			return graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{
				"target": {{From: "implementation", To: "foreign", Kind: graph.EdgeImplements}},
			}}, nil
		}
		if rows, ok := exploreBoundedEdgeIdentities(
			context.Background(), reader, []string{"target"}, []graph.EdgeKind{graph.EdgeImplements}, 1, exploreBoundedIncoming,
		); ok || rows != nil {
			t.Fatalf("incoming wrong-target projection accepted: ok=%v rows=%v", ok, rows)
		}
	})
}

func TestImplementationRecoveryRejectsIncomingAndHydrationFaultsAtomically(t *testing.T) {
	_, base := newStorageFixtureServer(t)
	interfaceSeed := exploreTarget{node: base.GetNode("repo/storage/storage.go::Storage"), score: 1}

	t.Run("incoming partial error", func(t *testing.T) {
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.incoming = func(ctx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
			projection, err := base.FindIncomingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
			if err != nil {
				return graph.BoundedEdgeIdentityProjection{}, err
			}
			return projection, errors.New("after partial projection")
		}
		got := exploreImplementationTestServer(reader).expandImplementationTargets(
			context.Background(), []exploreTarget{interfaceSeed}, graph.LocalizationNodeScope{},
		)
		if len(got) != 1 {
			t.Fatalf("incoming partial error returned %d targets", len(got))
		}
	})

	for _, test := range []struct {
		name   string
		lookup func(context.Context, []string) (map[string]*graph.Node, error)
	}{
		{name: "full map plus error", lookup: func(_ context.Context, ids []string) (map[string]*graph.Node, error) {
			return base.GetNodesByIDs(ids), errors.New("hydration failed")
		}},
		{name: "full map plus cancellation", lookup: func(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
			if cancel, ok := ctx.Value(exploreImplementationCancelKey{}).(context.CancelFunc); ok {
				cancel()
			}
			return base.GetNodesByIDs(ids), nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &exploreImplementationFaultReader{Reader: base, nodes: test.lookup}
			ctx := context.Background()
			if test.name == "full map plus cancellation" {
				cancelCtx, cancel := context.WithCancel(ctx)
				ctx = context.WithValue(cancelCtx, exploreImplementationCancelKey{}, context.CancelFunc(cancel))
			}
			got := exploreImplementationTestServer(reader).expandImplementationTargets(
				ctx, []exploreTarget{interfaceSeed}, graph.LocalizationNodeScope{},
			)
			if len(got) != 1 {
				t.Fatalf("faulted hydration returned %d targets", len(got))
			}
		})
	}

	t.Run("owner fold partial error", func(t *testing.T) {
		_, foldBase := newOwnerFoldFixture(t)
		reader := &exploreImplementationFaultReader{Reader: foldBase}
		reader.outgoing = func(ctx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
			projection, err := foldBase.FindOutgoingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
			if err != nil {
				return graph.BoundedEdgeIdentityProjection{}, err
			}
			return projection, errors.New("after partial projection")
		}
		members := []exploreTarget{
			{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.serialize")},
			{node: foldBase.GetNode("repo/mw/persist.ts::PersistOptions.hydrate")},
		}
		if got := exploreImplementationTestServer(reader).foldMemberOwners(context.Background(), members, graph.LocalizationNodeScope{}); len(got) != len(members) {
			t.Fatalf("faulted owner fold returned %d targets", len(got))
		}
	})
}

type exploreImplementationCancelKey struct{}

func TestExpandImplementationTargetsStageFaultSemantics(t *testing.T) {
	for _, test := range []struct {
		name         string
		stageLimit   int
		mode         string
		wantOriginal bool
	}{
		{name: "implementor partial error", stageLimit: exploreImplImplementorLimit, mode: "error", wantOriginal: true},
		{name: "implementor malformed", stageLimit: exploreImplImplementorLimit, mode: "malformed", wantOriginal: true},
		{name: "implementor canceled", stageLimit: exploreImplImplementorLimit, mode: "cancel", wantOriginal: true},
		{name: "member partial error falls back", stageLimit: exploreImplMemberRefinementLimit, mode: "error"},
		{name: "member malformed falls back", stageLimit: exploreImplMemberRefinementLimit, mode: "malformed"},
		{name: "member truncated falls back", stageLimit: exploreImplMemberRefinementLimit, mode: "truncated"},
		{name: "member canceled aborts lane", stageLimit: exploreImplMemberRefinementLimit, mode: "cancel", wantOriginal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, base := newStorageFixtureServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			reader := &exploreImplementationFaultReader{Reader: base}
			reader.incoming = func(callCtx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
				projection, err := base.FindIncomingEdgeIdentitiesBounded(callCtx, ids, kinds, limit)
				if err != nil || limit != test.stageLimit {
					return projection, err
				}
				switch test.mode {
				case "error":
					return projection, errors.New("stage failed after projection")
				case "cancel":
					cancel()
					return projection, nil
				case "malformed":
					return graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{
						ids[0]: {{From: "foreign", To: ids[0], Kind: kinds[0]}},
					}}, nil
				case "truncated":
					return graph.BoundedEdgeIdentityProjection{Truncated: map[string]bool{ids[0]: true}}, nil
				default:
					return projection, nil
				}
			}
			seed := exploreTarget{node: base.GetNode("repo/storage/storage.go::Storage.load"), score: 1}
			got := exploreImplementationTestServer(reader).expandImplementationTargets(
				ctx, []exploreTarget{seed}, graph.LocalizationNodeScope{},
			)
			if test.wantOriginal {
				if len(got) != 1 || got[0].node.ID != seed.node.ID {
					t.Fatalf("faulted stage returned partial targets: %#v", got)
				}
				return
			}
			if len(got) < 2 {
				t.Fatalf("member fault lost concrete-type fallback: %#v", got)
			}
			for _, target := range got[1:] {
				if target.node == nil || target.node.Kind != graph.KindType {
					t.Fatalf("member fault admitted non-type refinement: %#v", target.node)
				}
			}
		})
	}
}

func TestFoldMemberOwnersRelationFaultsAreAtomic(t *testing.T) {
	for _, mode := range []string{"error", "cancel", "malformed", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			_, base := newOwnerFoldFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			reader := &exploreImplementationFaultReader{Reader: base}
			reader.outgoing = func(callCtx context.Context, ids []string, kinds []graph.EdgeKind, limit int) (graph.BoundedEdgeIdentityProjection, error) {
				projection, err := base.FindOutgoingEdgeIdentitiesBounded(callCtx, ids, kinds, limit)
				if err != nil {
					return projection, err
				}
				switch mode {
				case "error":
					return projection, errors.New("fold projection failed")
				case "cancel":
					cancel()
					return projection, nil
				case "malformed":
					return graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{
						ids[0]: {{From: "foreign", To: "owner", Kind: graph.EdgeMemberOf}},
					}}, nil
				case "truncated":
					return graph.BoundedEdgeIdentityProjection{Truncated: map[string]bool{ids[0]: true}}, nil
				}
				return projection, nil
			}
			members := []exploreTarget{
				{node: base.GetNode("repo/mw/persist.ts::PersistOptions.serialize")},
				{node: base.GetNode("repo/mw/persist.ts::PersistOptions.hydrate")},
			}
			got := exploreImplementationTestServer(reader).foldMemberOwners(ctx, members, graph.LocalizationNodeScope{})
			if len(got) != len(members) || got[0].node.ID != members[0].node.ID {
				t.Fatalf("faulted fold returned partial mutation: %#v", got)
			}
		})
	}
}
