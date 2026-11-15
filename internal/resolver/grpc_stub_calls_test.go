package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// grpcTestGraph is a builder for the minimal graph shape the
// ResolveGRPCStubCalls pass consumes: a client function with a
// grpc.stub call edge, and a server-side handler discoverable via
// registration and/or interface satisfaction.
type grpcTestGraph struct {
	g graph.Store
}

func newGRPCTestGraph() *grpcTestGraph { return &grpcTestGraph{g: graph.New()} }

// addCaller adds a client-side function node.
func (b *grpcTestGraph) addCaller(id, filePath, repo string) {
	b.g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: lastSeg(id), FilePath: filePath, RepoPrefix: repo})
}

// addStubCall adds the grpc.stub EdgeCalls placeholder edge from caller.
func (b *grpcTestGraph) addStubCall(callerID, service, method, filePath string) *graph.Edge {
	e := &graph.Edge{
		From: callerID, To: grpcStubPlaceholder(service, method),
		Kind: graph.EdgeCalls, FilePath: filePath, Line: 10,
		Meta: map[string]any{"via": "grpc.stub", "grpc_service": service, "grpc_method": method},
	}
	b.g.AddEdge(e)
	return e
}

// addServerImpl adds a server impl type plus one method per name, wired
// with EdgeMemberOf. Returns the method node IDs keyed by name.
func (b *grpcTestGraph) addServerImpl(typeName, dir, repo string, methods ...string) map[string]string {
	typeID := dir + "/impl.go::" + typeName
	b.g.AddNode(&graph.Node{ID: typeID, Kind: graph.KindType, Name: typeName, FilePath: dir + "/impl.go", RepoPrefix: repo})
	out := map[string]string{}
	for _, m := range methods {
		mid := dir + "/impl.go::" + typeName + "." + m
		b.g.AddNode(&graph.Node{
			ID: mid, Kind: graph.KindMethod, Name: m, FilePath: dir + "/impl.go", RepoPrefix: repo,
			Meta: map[string]any{"receiver": typeName},
		})
		b.g.AddEdge(&graph.Edge{From: mid, To: typeID, Kind: graph.EdgeMemberOf, FilePath: dir + "/impl.go", Line: 1})
		out[m] = mid
	}
	return out
}

// addRegistration adds the server-side registration call edge that
// names typeName as the impl for service.
func (b *grpcTestGraph) addRegistration(service, typeName, regFuncID, regFilePath, repo string) {
	b.g.AddNode(&graph.Node{ID: regFuncID, Kind: graph.KindFunction, Name: lastSeg(regFuncID), FilePath: regFilePath, RepoPrefix: repo})
	b.g.AddEdge(&graph.Edge{
		From: regFuncID, To: "unresolved::extern::example.com/proto::Register" + service + "Server",
		Kind: graph.EdgeCalls, FilePath: regFilePath, Line: 5,
		Meta: map[string]any{"grpc_register_service": service, "grpc_register_impl": typeName},
	})
}

// addServerInterface adds a `<Service>Server` interface and an
// EdgeImplements edge from the impl type to it.
func (b *grpcTestGraph) addServerInterface(service, implTypeID, ifaceDir, repo string) {
	ifaceID := ifaceDir + "/grpc.pb.go::" + service + "Server"
	b.g.AddNode(&graph.Node{ID: ifaceID, Kind: graph.KindInterface, Name: service + "Server", FilePath: ifaceDir + "/grpc.pb.go", RepoPrefix: repo})
	b.g.AddEdge(&graph.Edge{From: implTypeID, To: ifaceID, Kind: graph.EdgeImplements, FilePath: ifaceDir + "/grpc.pb.go", Line: 1})
}

func lastSeg(id string) string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' || id[i] == '.' || id[i] == '/' {
			return id[i+1:]
		}
	}
	return id
}

func TestResolveGRPCStubCalls_Registration(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	methods := b.addServerImpl("userServer", "svc", "svc", "GetUser", "ListUsers")
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, methods["GetUser"], call.To, "stub call must land on the registered handler")
	assert.Equal(t, graph.OriginASTResolved, call.Origin)
	assert.Equal(t, 0.9, call.Confidence)
	assert.Equal(t, "EXTRACTED", call.ConfidenceLabel)
	assert.Equal(t, graph.OriginASTResolved, call.Meta["grpc_resolution"])

	// Edge buckets stay consistent after ReindexEdge.
	assert.Equal(t, call, firstOutEdgeByKind(b.g, "cli/main.go::run", graph.EdgeCalls))
	require.Len(t, b.g.GetInEdges(methods["GetUser"]), 1, "handler must see the inbound call edge")
}

func TestResolveGRPCStubCalls_InterfaceFallback(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	methods := b.addServerImpl("userServer", "svc", "svc", "GetUser")
	b.addServerInterface("UserService", "svc/impl.go::userServer", "svc", "svc")
	// No registration — interface satisfaction is the only signal.

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, methods["GetUser"], call.To)
	assert.Equal(t, graph.OriginASTInferred, call.Origin)
	assert.Equal(t, 0.7, call.Confidence)
}

func TestResolveGRPCStubCalls_RegistrationPreferredOverInterface(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	// The real handler, found via registration.
	real := b.addServerImpl("userServer", "svc", "svc", "GetUser")
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")
	// A decoy type also structurally implements the interface.
	decoy := b.addServerImpl("decoyServer", "decoy", "svc", "GetUser")
	b.addServerInterface("UserService", "decoy/impl.go::decoyServer", "decoy", "svc")

	ResolveGRPCStubCalls(b.g)
	assert.Equal(t, real["GetUser"], call.To, "registration signal must win over interface signal")
	assert.NotEqual(t, decoy["GetUser"], call.To)
}

func TestResolveGRPCStubCalls_UnimplementedSkipped(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	// Only the generated Unimplemented stub implements the interface;
	// the interface signal must skip it and resolve nothing.
	b.addServerImpl("UnimplementedUserServiceServer", "svc", "svc", "GetUser")
	b.addServerInterface("UserService", "svc/impl.go::UnimplementedUserServiceServer", "svc", "svc")

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, grpcStubPlaceholder("UserService", "GetUser"), call.To)
}

func TestResolveGRPCStubCalls_NoHandlerStaysPlaceholder(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, grpcStubPlaceholder("UserService", "GetUser"), call.To)
	assert.Empty(t, call.Origin)
}

func TestResolveGRPCStubCalls_Idempotent(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	methods := b.addServerImpl("userServer", "svc", "svc", "GetUser")
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")

	first := ResolveGRPCStubCalls(b.g)
	second := ResolveGRPCStubCalls(b.g)
	third := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 1, first)
	assert.Equal(t, 1, second)
	assert.Equal(t, 1, third)
	assert.Equal(t, methods["GetUser"], call.To)
	// No duplicate inbound edges accreted across re-runs.
	require.Len(t, b.g.GetInEdges(methods["GetUser"]), 1)
}

func TestResolveGRPCStubCalls_ReorphanWhenSignalLost(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	b.addServerImpl("userServer", "svc", "svc", "GetUser")
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")

	ResolveGRPCStubCalls(b.g)
	require.NotEqual(t, grpcStubPlaceholder("UserService", "GetUser"), call.To)

	// The wiring file is reindexed and no longer registers the server
	// (and there is no interface-satisfaction fallback). The handler
	// node and the client call edge both still exist, but the pass can
	// no longer discover a handler — the edge must re-orphan.
	b.g.EvictFile("svc/main.go")

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, grpcStubPlaceholder("UserService", "GetUser"), call.To, "edge must re-orphan to the placeholder")
	assert.Empty(t, call.Origin)
	assert.Empty(t, call.ConfidenceLabel)
	_, hasRes := call.Meta["grpc_resolution"]
	assert.False(t, hasRes, "grpc_resolution meta must be cleared on re-orphan")
}

func TestResolveGRPCStubCalls_AmbiguousUnresolved(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	// Two distinct impl types in the same repo both expose GetUser via
	// the interface signal — ambiguous, must not resolve.
	b.addServerImpl("serverA", "a", "svc", "GetUser")
	b.addServerImpl("serverB", "b", "svc", "GetUser")
	b.addServerInterface("UserService", "a/impl.go::serverA", "a", "svc")
	b.addServerInterface("UserService", "b/impl.go::serverB", "b", "svc")

	resolved := ResolveGRPCStubCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, grpcStubPlaceholder("UserService", "GetUser"), call.To)
}

func TestResolveGRPCStubCalls_SameRepoPreference(t *testing.T) {
	b := newGRPCTestGraph()
	// Caller lives in repo "svc" alongside one of two handler impls.
	b.addCaller("svc/client.go::run", "svc/client.go", "svc")
	call := b.addStubCall("svc/client.go::run", "UserService", "GetUser", "svc/client.go")
	local := b.addServerImpl("localServer", "svc", "svc", "GetUser")
	b.addServerInterface("UserService", "svc/impl.go::localServer", "svc", "svc")
	b.addServerImpl("remoteServer", "other", "other", "GetUser")
	b.addServerInterface("UserService", "other/impl.go::remoteServer", "other", "other")

	ResolveGRPCStubCalls(b.g)
	assert.Equal(t, local["GetUser"], call.To, "same-repo handler must win the tie-break")
}

func TestResolveGRPCStubCalls_CrossRepo(t *testing.T) {
	// Client in repo "cli", handler in repo "svc": the resolved
	// EdgeCalls edge then flows through DetectCrossRepoEdges.
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	methods := b.addServerImpl("userServer", "svc", "svc", "GetUser")
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")

	ResolveGRPCStubCalls(b.g)
	require.Equal(t, methods["GetUser"], call.To)

	emitted := detectCrossRepoEdges(t, b.g)
	assert.Equal(t, 1, emitted, "resolved cross-repo gRPC call must materialise a cross_repo_calls edge")
	cr := firstOutEdgeByKind(b.g, "cli/main.go::run", graph.EdgeCrossRepoCalls)
	require.NotNil(t, cr)
	assert.Equal(t, methods["GetUser"], cr.To)
}
