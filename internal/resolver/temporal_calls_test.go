package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// temporalTestGraph builds the minimal shape ResolveTemporalCalls
// consumes: a workflow function with a temporal.stub call edge, plus
// either a Go register-call edge or a Java @ActivityInterface +
// EdgeImplements chain that names the activity.
type temporalTestGraph struct {
	g graph.Store
}

func newTemporalTestGraph() *temporalTestGraph { return &temporalTestGraph{g: graph.New()} }

// addGoFunc adds a Go function or method node.
func (b *temporalTestGraph) addGoFunc(id, name, filePath, repo string) *graph.Node {
	n := &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: filePath, RepoPrefix: repo, Language: "go",
	}
	b.g.AddNode(n)
	return n
}

// addStubCall adds a Temporal stub-call placeholder edge from caller.
func (b *temporalTestGraph) addStubCall(callerID, kind, name, filePath string) *graph.Edge {
	e := &graph.Edge{
		From: callerID, To: temporalStubPlaceholder(kind, name),
		Kind: graph.EdgeCalls, FilePath: filePath, Line: 10,
		Meta: map[string]any{
			"via":           "temporal.stub",
			"temporal_kind": kind,
			"temporal_name": name,
		},
	}
	b.g.AddEdge(e)
	return e
}

// addStubCallEnvDefault adds a Temporal stub-call edge whose name was
// resolved from an env-var-with-literal-default variable
// (temporal_name_origin=env_default). The resolver must still land it on
// the registered handler but at the speculative tier (the runtime env
// override may differ from the default).
func (b *temporalTestGraph) addStubCallEnvDefault(callerID, kind, name, filePath string) *graph.Edge {
	e := b.addStubCall(callerID, kind, name, filePath)
	e.Meta["temporal_name_origin"] = "env_default"
	return e
}

// addGoRegister adds a Go `worker.RegisterActivity(F)` edge: an
// EdgeCalls edge from the worker-setup function to a placeholder,
// carrying the temporal.register meta the resolver consumes.
func (b *temporalTestGraph) addGoRegister(callerID, kind, name, filePath string) *graph.Edge {
	e := &graph.Edge{
		From: callerID, To: "unresolved::extern::go.temporal.io/sdk/worker::Register" + capitalise(kind),
		Kind: graph.EdgeCalls, FilePath: filePath, Line: 5,
		Meta: map[string]any{
			"via":           "temporal.register",
			"temporal_kind": kind,
			"temporal_name": name,
		},
	}
	b.g.AddEdge(e)
	return e
}

// addJavaInterface adds an interface node tagged with @ActivityInterface
// (or @WorkflowInterface) plus its method nodes (flat, no receiver) and
// the EdgeAnnotated edge to the annotation node the Java extractor
// would emit.
func (b *temporalTestGraph) addJavaInterface(ifaceID, name, filePath string, annoID string, methods ...string) (ifaceNode *graph.Node, methodNodes map[string]*graph.Node) {
	ifaceNode = &graph.Node{
		ID: ifaceID, Kind: graph.KindInterface, Name: name,
		FilePath: filePath, Language: "java",
		StartLine: 10, EndLine: 30,
	}
	b.g.AddNode(ifaceNode)

	annoNode := b.g.GetNode(annoID)
	if annoNode == nil {
		b.g.AddNode(&graph.Node{
			ID: annoID, Kind: graph.KindType, Name: lastSeg(annoID),
			FilePath: filePath, Language: "java",
			Meta: map[string]any{"kind": "annotation", "synthetic": true},
		})
	}
	b.g.AddEdge(&graph.Edge{From: ifaceID, To: annoID, Kind: graph.EdgeAnnotated, FilePath: filePath, Line: 9})

	methodNodes = map[string]*graph.Node{}
	for i, m := range methods {
		mid := filePath + "::" + m
		mn := &graph.Node{
			ID: mid, Kind: graph.KindMethod, Name: m,
			FilePath: filePath, Language: "java",
			StartLine: 11 + i, EndLine: 11 + i,
		}
		b.g.AddNode(mn)
		methodNodes[m] = mn
	}
	return ifaceNode, methodNodes
}

// addJavaImpl adds a Java implementation class with the named methods
// and the EdgeImplements edge from class → interface.
func (b *temporalTestGraph) addJavaImpl(classID, name, filePath, ifaceID string, methods ...string) (classNode *graph.Node, methodNodes map[string]*graph.Node) {
	classNode = &graph.Node{
		ID: classID, Kind: graph.KindType, Name: name,
		FilePath: filePath, Language: "java",
	}
	b.g.AddNode(classNode)
	b.g.AddEdge(&graph.Edge{From: classID, To: ifaceID, Kind: graph.EdgeImplements, FilePath: filePath, Line: 1})

	methodNodes = map[string]*graph.Node{}
	for _, m := range methods {
		mid := filePath + "::" + name + "." + m
		mn := &graph.Node{
			ID: mid, Kind: graph.KindMethod, Name: m,
			FilePath: filePath, Language: "java",
			Meta: map[string]any{"receiver": name},
		}
		b.g.AddNode(mn)
		methodNodes[m] = mn
	}
	return classNode, methodNodes
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// addWrapperStubCall adds a Temporal wrapper stub edge: from=wrapperID, to=placeholder,
// with temporal_name_param set so resolveTemporalWrapperCalls recognises it as a wrapper.
func (b *temporalTestGraph) addWrapperStubCall(wrapperID, kind, paramName, filePath string) *graph.Edge {
	e := &graph.Edge{
		From: wrapperID, To: temporalStubPlaceholder(kind, paramName),
		Kind: graph.EdgeCalls, FilePath: filePath, Line: 5,
		Meta: map[string]any{
			"via":                 "temporal.stub",
			"temporal_kind":       kind,
			"temporal_name":       paramName,
			"temporal_name_param": paramName,
		},
	}
	b.g.AddEdge(e)
	return e
}

// addCallerEdgeWithArgNames adds an EdgeCalls from callerID to calleeID carrying arg_names.
func (b *temporalTestGraph) addCallerEdgeWithArgNames(callerID, calleeID string, argNames []string, line int) *graph.Edge {
	e := &graph.Edge{
		From: callerID, To: calleeID,
		Kind: graph.EdgeCalls, FilePath: "wf/caller.go", Line: line,
		Meta: map[string]any{"arg_names": argNames},
	}
	b.g.AddEdge(e)
	return e
}

func TestResolveTemporalCalls_WrapperFollowing(t *testing.T) {
	// Graph layout:
	//   execAct (wrapper): temporal.stub edge with temporal_name_param="name"
	//   execAct#param:name: KindParam at position 1 (0-indexed: ctx=0, name=1)
	//   OrderWorkflow: calls execAct with arg_names=["ctx", "ChargeCard", "in"] at position 1
	//   ChargeCard: registered activity
	b := newTemporalTestGraph()

	// The wrapper function
	b.addGoFunc("wf/wrap.go::execAct", "execAct", "wf/wrap.go", "svc")
	// Add the #param:name node at position 1
	b.g.AddNode(&graph.Node{
		ID: "wf/wrap.go::execAct#param:name", Kind: graph.KindParam, Name: "name",
		FilePath: "wf/wrap.go", Language: "go",
		Meta: map[string]any{"position": 1},
	})
	// The wrapper stub edge
	b.addWrapperStubCall("wf/wrap.go::execAct", "activity", "name", "wf/wrap.go")

	// The caller
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	b.addCallerEdgeWithArgNames("wf/workflow.go::OrderWorkflow", "wf/wrap.go::execAct",
		[]string{"ctx", "ChargeCard", "in"}, 15)

	// The registered activity
	activity := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "ChargeCard", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.GreaterOrEqual(t, resolved, 1, "wrapper-following must resolve at least one stub")

	// The OrderWorkflow must now have a temporal.stub edge pointing at ChargeCard
	var wrapperStub *graph.Edge
	for _, e := range b.g.GetOutEdges("wf/workflow.go::OrderWorkflow") {
		if e == nil || e.Meta == nil {
			continue
		}
		if e.Meta["via"] == "temporal.stub" && e.Meta["temporal_name"] == "ChargeCard" {
			wrapperStub = e
			break
		}
	}
	require.NotNil(t, wrapperStub, "caller must have a synthesized temporal.stub edge for ChargeCard")
	assert.Equal(t, activity.ID, wrapperStub.To,
		"synthesized stub must resolve to the registered ChargeCard activity")
	assert.Equal(t, "execAct", wrapperStub.Meta["temporal_via_wrapper"],
		"synthesized edge must carry the wrapper name")
}

// --- Go-side tests --------------------------------------------------

func TestResolveTemporalCalls_GoActivityRegistration(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::OrderWorkflow", "activity", "ChargeCard", "wf/workflow.go")
	activity := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setupWorker", "setupWorker", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setupWorker", "activity", "ChargeCard", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, activity.ID, call.To, "stub call must land on the registered activity")
	assert.Equal(t, graph.OriginASTResolved, call.Origin)
	assert.Equal(t, 0.9, call.Confidence)
	assert.Equal(t, "EXTRACTED", call.ConfidenceLabel)
	assert.Equal(t, graph.OriginASTResolved, call.Meta["temporal_resolution"])

	assert.Equal(t, "activity", activity.Meta["temporal_role"], "registered activity must carry temporal_role meta")
	assert.Equal(t, "ChargeCard", activity.Meta["temporal_name"])

	require.Len(t, b.g.GetInEdges(activity.ID), 1, "activity must see the inbound call edge")
}

func TestResolveTemporalCalls_EnvDefaultResolvesSpeculative(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	call := b.addStubCallEnvDefault("wf/workflow.go::OrderWorkflow", "activity", "ChargeCard", "wf/workflow.go")
	activity := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setupWorker", "setupWorker", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setupWorker", "activity", "ChargeCard", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, activity.ID, call.To, "env-default stub must still land on the registered activity")
	assert.Equal(t, graph.OriginSpeculative, call.Origin, "env-default resolution must be speculative tier")
	assert.Less(t, call.Confidence, 0.5, "speculative confidence must be below the inferred threshold")
	assert.Equal(t, true, call.Meta[graph.MetaSpeculative], "env-default edge must be hidden-by-default")
}

func TestResolveTemporalCalls_AllowlistedEnvDefaultResolvesInferred(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	call := b.addStubCallEnvDefault("wf/workflow.go::OrderWorkflow", "activity", "ChargeCard", "wf/workflow.go")
	call.Meta["temporal_env_source"] = "allowlist"
	activity := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setupWorker", "setupWorker", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setupWorker", "activity", "ChargeCard", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, activity.ID, call.To)
	assert.Equal(t, graph.OriginASTInferred, call.Origin)
	assert.GreaterOrEqual(t, call.Confidence, 0.6)
	assert.NotEqual(t, true, call.Meta[graph.MetaSpeculative], "allow-listed default must be visible")
}

func TestResolveTemporalCalls_EnvDefaultUnresolvedStaysPlaceholder(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "svc")
	call := b.addStubCallEnvDefault("wf/workflow.go::WF", "activity", "MissingActivity", "wf/workflow.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, temporalStubPlaceholder("activity", "MissingActivity"), call.To)
	_, speculative := call.Meta[graph.MetaSpeculative]
	assert.False(t, speculative, "unresolved env-default edge must not carry the speculative flag")
}

// TestResolveTemporalCalls_CrossRepoTestStubSuppressed reproduces the one
// confirmed false positive from the L1 corpus audit: a dispatch in a service
// repo would otherwise resolve to a REGISTERED activity that lives only in a
// *_test.go fixture of an UNRELATED workflow repo. That target is a test mock,
// not the real implementation, so the edge is spurious. eligibleTemporalCandidates
// drops the cross-repo *_test.go candidate, leaving the dispatch unresolved
// rather than minting the cross-repo test-stub edge. (Without the filter this
// lone candidate would resolve via lookup's unique-overall branch — that
// asymmetry is what makes this test load-bearing.)
func TestResolveTemporalCalls_CrossRepoTestStubSuppressed(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("svc-beta/service.go::CatalogWorkflow", "CatalogWorkflow", "svc-beta/service.go", "svc-beta")
	call := b.addStubCall("svc-beta/service.go::CatalogWorkflow", "activity", "GetCatalogActivity", "svc-beta/service.go")
	// The only candidate named GetCatalogActivity is a REGISTERED test stub in
	// a different repo's *_test.go — a mock, not the real activity.
	b.addGoFunc("wf-theta/workflow_test.go::GetCatalogActivity", "GetCatalogActivity", "wf-theta/workflow_test.go", "wf-theta")
	b.addGoFunc("wf-theta/main.go::setupWorker", "setupWorker", "wf-theta/main.go", "wf-theta")
	b.addGoRegister("wf-theta/main.go::setupWorker", "activity", "GetCatalogActivity", "wf-theta/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 0, resolved, "cross-repo *_test.go stub must not be treated as a real handler")
	assert.Equal(t, temporalStubPlaceholder("activity", "GetCatalogActivity"), call.To,
		"dispatch must stay an unresolved placeholder, not point at the cross-repo test mock")
}

// TestResolveTemporalCalls_SameRepoTestActivityResolves guards the cross-repo
// filter from over-reaching: a test workflow dispatching a REGISTERED activity
// that lives in the SAME repo's *_test.go is the overwhelmingly common (and
// correct) case — the filter must leave it eligible so it still resolves.
func TestResolveTemporalCalls_SameRepoTestActivityResolves(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("svc/wf_test.go::OrderTestWorkflow", "OrderTestWorkflow", "svc/wf_test.go", "svc")
	call := b.addStubCall("svc/wf_test.go::OrderTestWorkflow", "activity", "PostBillingActivity", "svc/wf_test.go")
	activity := b.addGoFunc("svc/wf_test.go::PostBillingActivity", "PostBillingActivity", "svc/wf_test.go", "svc")
	b.addGoFunc("svc/main.go::setupWorker", "setupWorker", "svc/main.go", "svc")
	b.addGoRegister("svc/main.go::setupWorker", "activity", "PostBillingActivity", "svc/main.go")

	ResolveTemporalCalls(b.g)
	assert.Equal(t, activity.ID, call.To, "same-repo *_test.go activity must stay eligible and resolve")
}

func TestResolveTemporalCalls_GoChildWorkflowRegistration(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("a/parent.go::ParentWorkflow", "ParentWorkflow", "a/parent.go", "svc")
	call := b.addStubCall("a/parent.go::ParentWorkflow", "workflow", "ChildWorkflow", "a/parent.go")
	child := b.addGoFunc("a/child.go::ChildWorkflow", "ChildWorkflow", "a/child.go", "svc")
	b.addGoFunc("a/main.go::setup", "setup", "a/main.go", "svc")
	b.addGoRegister("a/main.go::setup", "workflow", "ChildWorkflow", "a/main.go")

	ResolveTemporalCalls(b.g)
	assert.Equal(t, child.ID, call.To)
	assert.Equal(t, "workflow", child.Meta["temporal_role"])
}

func TestResolveTemporalCalls_GoNoRegistrationStaysPlaceholder(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::WF", "activity", "MissingActivity", "wf/workflow.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, temporalStubPlaceholder("activity", "MissingActivity"), call.To)
	assert.Empty(t, call.Origin)
}

func TestResolveTemporalCalls_GoIdempotent(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::WF", "activity", "Charge", "wf/workflow.go")
	activity := b.addGoFunc("wf/activity.go::Charge", "Charge", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "Charge", "wf/main.go")

	first := ResolveTemporalCalls(b.g)
	second := ResolveTemporalCalls(b.g)
	third := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, first)
	assert.Equal(t, 1, second)
	assert.Equal(t, 1, third)
	assert.Equal(t, activity.ID, call.To)
	require.Len(t, b.g.GetInEdges(activity.ID), 1, "no duplicate inbound edges across re-runs")
}

func TestResolveTemporalCalls_GoReorphanOnHandlerLost(t *testing.T) {
	// First settle the stub call onto a resolved handler, then mutate
	// the call's temporal_name so the next pass can't find a handler
	// for it — the edge must re-orphan to the placeholder and drop
	// its resolution metadata. The same code path runs when the real
	// daemon evicts a register file: the stub-call edge survives the
	// reindex, but the resolver no longer finds a target.
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::WF", "activity", "Charge", "wf/workflow.go")
	b.addGoFunc("wf/activity.go::Charge", "Charge", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "Charge", "wf/main.go")

	ResolveTemporalCalls(b.g)
	require.NotEqual(t, temporalStubPlaceholder("activity", "Charge"), call.To)
	require.Equal(t, graph.OriginASTResolved, call.Origin)

	// Re-target the stub call at an activity name nothing registers.
	call.Meta["temporal_name"] = "NoSuchActivity"

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 0, resolved)
	assert.Equal(t, temporalStubPlaceholder("activity", "NoSuchActivity"), call.To)
	assert.Empty(t, call.Origin)
	_, hasRes := call.Meta["temporal_resolution"]
	assert.False(t, hasRes, "temporal_resolution meta must be cleared on re-orphan")
}

func TestResolveTemporalCalls_GoSameRepoPreference(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("svc/workflow.go::WF", "WF", "svc/workflow.go", "svc")
	call := b.addStubCall("svc/workflow.go::WF", "activity", "Charge", "svc/workflow.go")
	local := b.addGoFunc("svc/activity.go::Charge", "Charge", "svc/activity.go", "svc")
	b.addGoFunc("other/activity.go::Charge", "Charge", "other/activity.go", "other")
	b.addGoFunc("svc/main.go::setup", "setup", "svc/main.go", "svc")
	b.addGoRegister("svc/main.go::setup", "activity", "Charge", "svc/main.go")

	ResolveTemporalCalls(b.g)
	assert.Equal(t, local.ID, call.To, "same-repo activity must win the tie-break")
}

func TestResolveTemporalCalls_GoLocalActivityFlagPreserved(t *testing.T) {
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::WF", "activity", "Lookup", "wf/workflow.go")
	call.Meta["temporal_local"] = true
	b.addGoFunc("wf/activity.go::Lookup", "Lookup", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "Lookup", "wf/main.go")

	ResolveTemporalCalls(b.g)
	assert.Equal(t, true, call.Meta["temporal_local"], "local-activity flag must survive the rewrite")
}

func TestResolveTemporalCalls_GoCrossRepoFlowsThroughDetector(t *testing.T) {
	// Workflow in repo "wf", activity in repo "acts", worker setup in
	// repo "wf". After resolution the cross-repo edge layer must
	// materialise a cross_repo_calls parallel edge.
	b := newTemporalTestGraph()
	b.addGoFunc("wf/workflow.go::WF", "WF", "wf/workflow.go", "wf")
	call := b.addStubCall("wf/workflow.go::WF", "activity", "Charge", "wf/workflow.go")
	activity := b.addGoFunc("acts/activity.go::Charge", "Charge", "acts/activity.go", "acts")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "wf")
	b.addGoRegister("wf/main.go::setup", "activity", "Charge", "wf/main.go")

	ResolveTemporalCalls(b.g)
	require.Equal(t, activity.ID, call.To)

	emitted := detectCrossRepoEdges(t, b.g)
	assert.GreaterOrEqual(t, emitted, 1, "resolved cross-repo Temporal call must materialise a cross_repo_calls edge")
	cr := firstOutEdgeByKind(b.g, "wf/workflow.go::WF", graph.EdgeCrossRepoCalls)
	require.NotNil(t, cr)
	assert.Equal(t, activity.ID, cr.To)
}

// --- Java-side tests ------------------------------------------------

func TestResolveTemporalCalls_JavaActivityInterfacePropagation(t *testing.T) {
	b := newTemporalTestGraph()
	iface, ifaceMethods := b.addJavaInterface(
		"OrderActivities.java::OrderActivities", "OrderActivities", "OrderActivities.java",
		javaActivityIfaceAnnoID, "chargeCard", "shipOrder",
	)
	_, implMethods := b.addJavaImpl(
		"OrderActivitiesImpl.java::OrderActivitiesImpl", "OrderActivitiesImpl",
		"OrderActivitiesImpl.java", iface.ID, "chargeCard", "shipOrder",
	)

	ResolveTemporalCalls(b.g)

	assert.Equal(t, "activity_interface", iface.Meta["temporal_role"])
	assert.Equal(t, "activity", ifaceMethods["chargeCard"].Meta["temporal_role"], "interface methods tagged")
	assert.Equal(t, "activity", ifaceMethods["shipOrder"].Meta["temporal_role"])
	assert.Equal(t, "activity", implMethods["chargeCard"].Meta["temporal_role"], "impl methods tagged via interface chain")
	assert.Equal(t, "activity", implMethods["shipOrder"].Meta["temporal_role"])
	// G2: an activity's canonical Temporal type is its method name with the
	// first letter capitalized (the Java SDK default), keyed off the same
	// string a Go RegisterActivity would use.
	assert.Equal(t, "ChargeCard", ifaceMethods["chargeCard"].Meta["temporal_name"],
		"activity canonical name is the capitalized method name")
	assert.Equal(t, "ChargeCard", implMethods["chargeCard"].Meta["temporal_name"])
}

func TestResolveTemporalCalls_JavaWorkflowInterfacePropagation(t *testing.T) {
	b := newTemporalTestGraph()
	iface, ifaceMethods := b.addJavaInterface(
		"OrderWorkflow.java::OrderWorkflow", "OrderWorkflow", "OrderWorkflow.java",
		javaWorkflowIfaceAnnoID, "processOrder",
	)
	_, implMethods := b.addJavaImpl(
		"OrderWorkflowImpl.java::OrderWorkflowImpl", "OrderWorkflowImpl",
		"OrderWorkflowImpl.java", iface.ID, "processOrder",
	)

	ResolveTemporalCalls(b.g)

	assert.Equal(t, "workflow_interface", iface.Meta["temporal_role"])
	assert.Equal(t, "workflow", ifaceMethods["processOrder"].Meta["temporal_role"])
	assert.Equal(t, "workflow", implMethods["processOrder"].Meta["temporal_role"])
	// G2: a workflow's canonical Temporal type is the interface simple
	// name (not the method name), so a Go service that starts this
	// workflow by type matches it.
	assert.Equal(t, "OrderWorkflow", ifaceMethods["processOrder"].Meta["temporal_name"],
		"workflow canonical name is the interface simple name")
	assert.Equal(t, "OrderWorkflow", implMethods["processOrder"].Meta["temporal_name"])
}

func TestJavaTemporalCanonicalNameHelpers(t *testing.T) {
	assert.Equal(t, "ChargeCard", javaAnnotationStringArg(`name = "ChargeCard"`, "name"))
	assert.Equal(t, "Foo_", javaAnnotationStringArg(`namePrefix = "Foo_"`, "namePrefix"))
	// "name" lookup must not match the "namePrefix" key.
	assert.Equal(t, "", javaAnnotationStringArg(`namePrefix = "Foo_"`, "name"))
	assert.Equal(t, "", javaAnnotationStringArg(``, "name"))

	assert.Equal(t, "ChargeCard", capitalizeASCII("chargeCard"))
	assert.Equal(t, "X", capitalizeASCII("x"))
	assert.Equal(t, "", capitalizeASCII(""))

	// explicit name wins; activity defaults to capitalized; others to bare.
	assert.Equal(t, "Override", javaMethodCanonicalName("activity", "chargeCard", `name = "Override"`))
	assert.Equal(t, "ChargeCard", javaMethodCanonicalName("activity", "chargeCard", ""))
	assert.Equal(t, "status", javaMethodCanonicalName("query", "status", ""))
	assert.Equal(t, "getStatus", javaMethodCanonicalName("query", "getStatus", ""))
}

func TestResolveTemporalCalls_JavaSignalAndQueryMethods(t *testing.T) {
	b := newTemporalTestGraph()
	// Method-level annotations on a workflow class — no interface-level
	// annotation; signal / query roles still get stamped.
	mid := "Workflow.java::handleSignal"
	method := &graph.Node{
		ID: mid, Kind: graph.KindMethod, Name: "handleSignal",
		FilePath: "Workflow.java", Language: "java",
		StartLine: 20,
	}
	b.g.AddNode(method)
	b.g.AddNode(&graph.Node{
		ID: javaSignalMethodID, Kind: graph.KindType, Name: "SignalMethod",
		FilePath: "Workflow.java", Language: "java",
		Meta: map[string]any{"kind": "annotation", "synthetic": true},
	})
	b.g.AddEdge(&graph.Edge{From: mid, To: javaSignalMethodID, Kind: graph.EdgeAnnotated, FilePath: "Workflow.java", Line: 19})

	qid := "Workflow.java::currentStatus"
	qmethod := &graph.Node{
		ID: qid, Kind: graph.KindMethod, Name: "currentStatus",
		FilePath: "Workflow.java", Language: "java",
		StartLine: 25,
	}
	b.g.AddNode(qmethod)
	b.g.AddNode(&graph.Node{
		ID: javaQueryMethodID, Kind: graph.KindType, Name: "QueryMethod",
		FilePath: "Workflow.java", Language: "java",
		Meta: map[string]any{"kind": "annotation", "synthetic": true},
	})
	b.g.AddEdge(&graph.Edge{From: qid, To: javaQueryMethodID, Kind: graph.EdgeAnnotated, FilePath: "Workflow.java", Line: 24})

	ResolveTemporalCalls(b.g)

	assert.Equal(t, "signal", method.Meta["temporal_role"])
	assert.Equal(t, "query", qmethod.Meta["temporal_role"])
}

func TestResolveTemporalCalls_JavaInterfaceMethodsScopedByLineRange(t *testing.T) {
	// Two interfaces in the same file: only the @ActivityInterface
	// methods get tagged, not the methods on the unrelated interface
	// that follows it.
	b := newTemporalTestGraph()
	b.addJavaInterface(
		"both.java::ActivityIface", "ActivityIface", "both.java",
		javaActivityIfaceAnnoID, "doWork",
	)
	// Inject a second, unrelated interface in the same file with
	// methods OUTSIDE the first interface's line range.
	other := &graph.Node{
		ID: "both.java::OtherIface", Kind: graph.KindInterface, Name: "OtherIface",
		FilePath: "both.java", Language: "java",
		StartLine: 40, EndLine: 60,
	}
	b.g.AddNode(other)
	otherMethod := &graph.Node{
		ID: "both.java::unrelated", Kind: graph.KindMethod, Name: "unrelated",
		FilePath: "both.java", Language: "java",
		StartLine: 45,
	}
	b.g.AddNode(otherMethod)

	ResolveTemporalCalls(b.g)

	_, hasRole := otherMethod.Meta["temporal_role"]
	assert.False(t, hasRole, "unrelated interface's methods must not get tagged")
}

func TestResolveTemporalCalls_RoleStampingIsIdempotent(t *testing.T) {
	b := newTemporalTestGraph()
	_, methods := b.addJavaInterface(
		"Acts.java::Acts", "Acts", "Acts.java",
		javaActivityIfaceAnnoID, "doIt",
	)
	for range 5 {
		ResolveTemporalCalls(b.g)
	}
	assert.Equal(t, "activity", methods["doIt"].Meta["temporal_role"])
}

func TestTemporalIndexLookup_LanguageGate(t *testing.T) {
	goNode := &graph.Node{ID: "go/a.go::ChargeCard", Name: "ChargeCard", Language: "go", RepoPrefix: "svc"}
	javaNode := &graph.Node{ID: "java/A.java::chargeCard", Name: "ChargeCard", Language: "java", RepoPrefix: "jsvc"}

	idx := &temporalIndex{byKindName: map[string][]*graph.Node{
		"activity::ChargeCard": {javaNode}, // only a Java candidate
	}}

	// A Go stub must NOT resolve onto a Java handler node even when the
	// Java entry is the unique overall candidate — that cross-language
	// match is the job of the dedicated join pass, not the stub resolver.
	id, _, _ := idx.lookup("activity", "ChargeCard", "svc", "go")
	assert.Empty(t, id, "go stub must not resolve to a java handler")

	// With a Go candidate present, the Go caller resolves to it (unique
	// within the caller's language).
	idx.byKindName["activity::ChargeCard"] = []*graph.Node{javaNode, goNode}
	id, origin, conf := idx.lookup("activity", "ChargeCard", "svc", "go")
	assert.Equal(t, goNode.ID, id)
	assert.Equal(t, graph.OriginASTResolved, origin)
	assert.Equal(t, 0.9, conf)

	// An unknown caller language keeps the language-agnostic
	// unique-overall fallback (no regression for callers with no lang).
	idx.byKindName["activity::Solo"] = []*graph.Node{javaNode}
	id, _, _ = idx.lookup("activity", "Solo", "", "")
	assert.Equal(t, javaNode.ID, id, "unknown caller lang keeps the unique-overall fallback")
}

func TestResolveTemporalCalls_CrossLangJavaStartsGoWorkflow(t *testing.T) {
	b := newTemporalTestGraph()
	// Go side: a workflow registered under the canonical name OrderWorkflow.
	b.addGoFunc("go/main.go::setup", "setup", "go/main.go", "gosvc")
	b.addGoRegister("go/main.go::setup", "workflow", "OrderWorkflow", "go/main.go")
	goWf := b.addGoFunc("go/wf.go::OrderWorkflow", "OrderWorkflow", "go/wf.go", "gosvc")

	// Java side (a DIFFERENT repo): a service that starts the workflow by
	// its canonical type name via a via=temporal.start consumer edge.
	javaCaller := &graph.Node{
		ID: "java/Svc.java::startOrder", Kind: graph.KindMethod, Name: "startOrder",
		FilePath: "java/Svc.java", RepoPrefix: "jsvc", Language: "java",
	}
	b.g.AddNode(javaCaller)
	startEdge := &graph.Edge{
		From: javaCaller.ID, To: temporalStubPlaceholder("workflow", "OrderWorkflow"),
		Kind: graph.EdgeCalls, FilePath: "java/Svc.java", Line: 10,
		Meta: map[string]any{
			"via": "temporal.start", "temporal_kind": "workflow", "temporal_name": "OrderWorkflow",
		},
	}
	b.g.AddEdge(startEdge)

	ResolveTemporalCalls(b.g)

	assert.Equal(t, goWf.ID, startEdge.To,
		"a Java start must cross-resolve to the Go workflow of the same canonical name")
	assert.Equal(t, graph.OriginSpeculative, startEdge.Origin,
		"a cross-language join lands at the speculative tier")
	assert.Equal(t, true, startEdge.Meta["temporal_cross_lang"])
	assert.Equal(t, true, startEdge.Meta[graph.MetaSpeculative], "cross-language edge is hidden by default")
}

func TestResolveTemporalCalls_RegisterNameOverride(t *testing.T) {
	b := newTemporalTestGraph()
	// Worker registers the impl ChargeCard under the override name "Charge"
	// (RegisterActivityWithOptions{Name: "Charge"}).
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	reg := b.addGoRegister("wf/main.go::setup", "activity", "ChargeCard", "wf/main.go")
	reg.Meta["temporal_registered_name"] = "Charge"
	impl := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")

	// A workflow dispatches by the OVERRIDE name, not the func name.
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::OrderWorkflow", "activity", "Charge", "wf/workflow.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, impl.ID, call.To,
		"a dispatch by the override name must land on the registered impl")
	assert.Equal(t, "Charge", impl.Meta["temporal_name"],
		"the impl is known under the registered (override) name")
	assert.Equal(t, "activity", impl.Meta["temporal_role"])
}

func TestResolveTemporalCalls_ExecutorFieldDispatch(t *testing.T) {
	b := newTemporalTestGraph()
	// Method stub edge with temporal_name_field + temporal_recv_type.
	b.addGoFunc("wf/executor.go::ActivityExecutor.Run", "Run", "wf/executor.go", "svc")
	methodStub := &graph.Edge{
		From: "wf/executor.go::ActivityExecutor.Run",
		To:   temporalStubPlaceholder("activity", "ActivityName"),
		Kind: graph.EdgeCalls, FilePath: "wf/executor.go", Line: 10,
		Meta: map[string]any{
			"via":                 "temporal.stub",
			"temporal_kind":       "activity",
			"temporal_name":       "ActivityName",
			"temporal_name_field": "ActivityName",
			"temporal_recv_type":  "ActivityExecutor",
		},
	}
	b.g.AddEdge(methodStub)

	// Executor-field marker edge: construction site.
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	markerEdge := &graph.Edge{
		From: "wf/main.go::setup",
		To:   "unresolved::temporal-executor::ActivityExecutor::ActivityName",
		Kind: graph.EdgeCalls, FilePath: "wf/main.go", Line: 5,
		Meta: map[string]any{
			"via":            "temporal.executor-field",
			"executor_type":  "ActivityExecutor",
			"executor_field": "ActivityName",
			"executor_value": "ChargeCard",
		},
	}
	b.g.AddEdge(markerEdge)

	// Registered activity.
	activity := b.addGoFunc("wf/activity.go::ChargeCard", "ChargeCard", "wf/activity.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "ChargeCard", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.GreaterOrEqual(t, resolved, 1, "ChargeCard must be resolved")
	assert.Equal(t, activity.ID, methodStub.To,
		"the method stub must be rewritten to the registered activity")
}

func TestResolveTemporalCalls_FuncReturningConstantDeref(t *testing.T) {
	// PURPOSE — a stub edge with temporal_name="GetChargeActivityName" must
	// resolve to the ChargeActivity handler when the func node has a sidecar
	// value "ChargeActivity" in the const-deref map.
	b := newTemporalTestGraph()

	// The workflow function that dispatches via GetChargeActivityName()
	b.addGoFunc("wf/workflow.go::OrderWorkflow", "OrderWorkflow", "wf/workflow.go", "svc")
	call := b.addStubCall("wf/workflow.go::OrderWorkflow", "activity", "GetChargeActivityName", "wf/workflow.go")

	// The func-returning-literal: GetChargeActivityName is a KindFunction node
	// whose ConstValue sidecar says "ChargeActivity"
	getNameFunc := &graph.Node{
		ID:       "wf/constants.go::GetChargeActivityName",
		Kind:     graph.KindFunction,
		Name:     "GetChargeActivityName",
		FilePath: "wf/constants.go",
		Language: "go",
	}
	b.g.AddNode(getNameFunc)

	// Inject its sidecar value via BulkSetConstantValues
	writer, ok := b.g.(graph.ConstantValueWriter)
	require.True(t, ok, "graph.New() must implement ConstantValueWriter")
	require.NoError(t, writer.BulkSetConstantValues("svc", []graph.ConstantValueRow{
		{NodeID: getNameFunc.ID, FilePath: getNameFunc.FilePath, Value: "ChargeActivity"},
	}))

	// The registered ChargeActivity handler
	activity := b.addGoFunc("wf/activity.go::ChargeActivity", "ChargeActivity", "wf/activity.go", "svc")
	b.addGoFunc("wf/main.go::setup", "setup", "wf/main.go", "svc")
	b.addGoRegister("wf/main.go::setup", "activity", "ChargeActivity", "wf/main.go")

	resolved := ResolveTemporalCalls(b.g)
	assert.Equal(t, 1, resolved)
	assert.Equal(t, activity.ID, call.To,
		"func-returning-constant dispatch must land on the registered ChargeActivity")
	assert.Equal(t, graph.OriginASTResolved, call.Origin)
}
