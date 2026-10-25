package indexer

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	sqlite "modernc.org/sqlite"
)

type claimedShadowDeleteProbe struct{ fire func() }

var claimedShadowDeleteObserver atomic.Pointer[claimedShadowDeleteProbe]
var claimedShadowDeleteRegister sync.Once

// Exercise the actual Building reservation -> runPass -> shadow replacement
// path. The old atomic positive-generation eviction holds every foreground
// checkout writer for all 1024 deliberately slow real SQL deletes.
func TestClaimedPopulatedBaseShadowReplacementAdmitsCheckoutWriter(t *testing.T) {
	claimedShadowDeleteRegister.Do(func() {
		require.NoError(t, sqlite.RegisterScalarFunction("gortex_test_claimed_shadow_delete", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			if p := claimedShadowDeleteObserver.Load(); p != nil {
				p.fire()
			}
			return int64(0), nil
		}))
	})
	builder, request, claim, logs := claimedCopyFixture(t)
	indexGenerationZero(t, builder, request)
	baseCount := builder.Store.AtGeneration(0).NodeCount()
	reserved, err := builder.Store.AtManagedGeneration(claim.GenerationID)
	require.NoError(t, err)
	nodes := make([]*graph.Node, 1024)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("%s::obsolete%d", request.RepoPrefix, i), Name: fmt.Sprintf("Obsolete%d", i), Kind: graph.KindFunction, FilePath: request.RepoPrefix + "::obsolete.go", RepoPrefix: request.RepoPrefix, Language: "go"}
	}
	require.NoError(t, reserved.AddBatchChecked(nodes, nil))
	_, foreground, err := builder.Store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "foreground-checkout", LayerID: "foreground-layer", GenerationKind: "commit", TreeOID: "foreground-tree", ConfigHash: "config", ExtractorVersions: `{"go":"1"}`, ResolverVersion: "resolver", CreatedAt: 2})
	require.NoError(t, err)
	conn, err := sql.Open("sqlite", builder.Store.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.Exec(fmt.Sprintf(`CREATE TRIGGER slow_claimed_shadow_delete BEFORE DELETE ON nodes WHEN OLD.view_gen=%d BEGIN SELECT gortex_test_claimed_shadow_delete(); END`, claim.GenerationID))
	require.NoError(t, err)
	entered := make(chan struct{})
	var once sync.Once
	var deleted atomic.Int32
	claimedShadowDeleteObserver.Store(&claimedShadowDeleteProbe{fire: func() {
		if deleted.Add(1) == 1 {
			once.Do(func() { close(entered) })
			// Keep the first transaction open until the foreground writer is
			// actually queued. The writer must run before a second delete chunk.
			require.Eventually(t, builder.Store.WriteWanted, 5*time.Second, time.Millisecond)
		}
	}})
	t.Cleanup(func() { claimedShadowDeleteObserver.Store(nil) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type result struct {
		err     error
		deleted int32
	}
	wrote := make(chan result, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case <-wrote:
		case <-time.After(testTimingSlack(5 * time.Second)):
			t.Error("foreground writer did not join")
		}
	})
	go func() {
		select {
		case <-entered:
		case <-ctx.Done():
			wrote <- result{err: ctx.Err()}
			return
		}
		err := foreground.AddBatchChecked([]*graph.Node{{ID: "foreground::Witness", Name: "Witness", Kind: graph.KindFunction, FilePath: "foreground::witness.go", RepoPrefix: "foreground"}}, nil)
		wrote <- result{err: err, deleted: deleted.Load()}
	}()
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: claim, RootPath: request.RootPath, WorkspaceID: request.WorkspaceID, ProjectID: request.ProjectID})
	require.NoError(t, err)
	require.Equal(t, claim.GenerationID, id)
	require.False(t, report.Coalesced)
	var r result
	select {
	case r = <-wrote:
	case <-time.After(testTimingSlack(5 * time.Second)):
		t.Fatal("foreground writer did not complete")
	}
	wrote <- r
	require.NoError(t, r.err)
	require.EqualValues(t, 256, r.deleted, "foreground writer did not run at the first delete-chunk release")
	require.NotNil(t, foreground.GetNode("foreground::Witness"))
	require.Equal(t, baseCount, builder.Store.AtGeneration(0).NodeCount())
	published := builder.Store.AtGeneration(id)
	for _, n := range nodes {
		require.Nil(t, published.GetNode(n.ID))
	}
	require.Positive(t, published.NodeCount())
	route, reason := claimedRoute(t, logs)
	require.Equal(t, "reparse_git_tree", route)
	require.Equal(t, "the reserved generation already carries payload", reason)
	drain := logs.FilterMessage("indexer: evicted stale generation rows before shadow drain").All()
	require.Len(t, drain, 1)
	require.Equal(t, claim.GenerationID, drain[0].ContextMap()["view_generation"])
}
