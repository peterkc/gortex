package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestSnapshotContractShapesAndCoverage(t *testing.T) {
	ctx := t.Context()
	worker, head, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	typeNode := &graph.Node{ID: "old/physical.go::T", RepoPrefix: "old", Kind: graph.KindType, FilePath: "old/physical.go", Meta: map[string]any{"ordinary": true}}
	otherType := &graph.Node{ID: "other/physical.go::T", RepoPrefix: "other", Kind: graph.KindType, FilePath: "other/physical.go"}
	worker.AddBatch([]*graph.Node{typeNode, otherType}, nil)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	pinned := stageRead(t, head)
	shape := &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "Z", Type: "string"}, {Name: "A", Type: "int"}}, Notes: []string{}}
	known := testContractOmission()
	unknown := RebuildContractOmission{RepoPrefix: known.RepoPrefix, FilePath: known.FilePath, Type: contracts.ContractEnv, RecordKey: HeadDigest{2}}
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(RebuildContractBatch{
		Shapes:  map[string]*contracts.Shape{typeNode.ID: shape},
		Omitted: []RebuildContractOmission{known, unknown},
	}), ContractCoverage{OmittedRecords: 2})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	current := stageRead(t, head)

	ids := []string{typeNode.ID, otherType.ID}
	capability := pinned
	_, err = capability.ContractCoverageContext(ctx, contracts.ReadScope{}, false)
	require.ErrorAs(t, err, new(*ErrStagePending))
	shapes, err := capability.ContractShapesContext(ctx, ids)
	require.ErrorAs(t, err, new(*ErrStagePending))
	require.Nil(t, shapes)
	_, err = capability.FullNodesByIDsContext(ctx, ids)
	require.ErrorAs(t, err, new(*ErrStagePending))
	require.NoError(t, pinned.ReadError(), "pending is not a storage error")

	capability = current
	shapes, err = capability.ContractShapesContext(ctx, ids)
	require.NoError(t, err)
	require.Equal(t, map[string]*contracts.Shape{typeNode.ID: shape}, shapes)
	full, err := capability.FullNodesByIDsContext(ctx, ids)
	require.NoError(t, err)
	want := *typeNode
	want.Meta = map[string]any{"ordinary": true, "shape": shape}
	require.Equal(t, &want, full[typeNode.ID])
	require.Equal(t, otherType, full[otherType.ID])
	full[typeNode.ID].Meta["ordinary"] = false
	require.Equal(t, pinned.GetNode(typeNode.ID), current.GetNode(typeNode.ID))
	require.Equal(t, typeNode, current.GetNode(typeNode.ID))

	for _, tc := range []struct {
		scope   contracts.ReadScope
		partial bool
	}{
		{scope: contracts.ReadScope{}, partial: true},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"old"}, FilePaths: []string{known.FilePath}, Types: []contracts.ContractType{known.Type}, ContractIDs: []string{known.ID}}, partial: true},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"other"}, ContractIDs: []string{known.ID}}},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"old"}, FilePaths: []string{"different.py"}}},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"old"}, Types: []contracts.ContractType{contracts.ContractGRPC}}},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"old"}, Types: []contracts.ContractType{known.Type}, ContractIDs: []string{"unrelated"}}},
		{scope: contracts.ReadScope{RepoPrefixes: []string{""}, FilePaths: []string{unknown.FilePath}, ContractIDs: []string{"env::POSSIBLE"}}},
		{scope: contracts.ReadScope{RepoPrefixes: []string{"old"}, FilePaths: []string{unknown.FilePath}, ContractIDs: []string{"env::POSSIBLE"}}, partial: true},
	} {
		positive, err := capability.ContractCoverageContext(ctx, tc.scope, false)
		require.NoError(t, err)
		require.Equal(t, tc.partial, positive, "%+v", tc.scope)
		negative, err := capability.ContractCoverageContext(ctx, tc.scope, true)
		if tc.partial {
			require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
		} else {
			require.NoError(t, err)
		}
		require.Equal(t, tc.partial, negative, "%+v", tc.scope)
	}

	reads := trackSnapshotReads(current)
	// The batch is sorted, so the good shape is decoded before the failure.
	reads.failPoint = string(contractShapeKey(otherType.ID))
	reads.failure = errors.New("injected second shape read failure")
	shapes, err = capability.ContractShapesContext(ctx, ids)
	require.ErrorIs(t, err, reads.failure)
	require.Nil(t, shapes)
	require.ErrorIs(t, current.ReadError(), reads.failure)
	_, err = capability.ContractCoverageContext(ctx, contracts.ReadScope{}, false)
	require.ErrorIs(t, err, reads.failure)
}

func TestSnapshotContractShapesBuiltEmpty(t *testing.T) {
	ctx := t.Context()
	worker, head, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	current := stageRead(t, head)
	shapes, err := current.ContractShapesContext(ctx, []string{"absent"})
	require.NoError(t, err)
	require.Empty(t, shapes)
	partial, err := current.ContractCoverageContext(ctx, contracts.ReadScope{}, true)
	require.NoError(t, err)
	require.False(t, partial)
}

func TestSnapshotContractsLegacyDerivedIsPending(t *testing.T) {
	ctx := t.Context()
	worker, head, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	derived, err := PrepareDeferredDerived(t.TempDir(), relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)
	_, err = stageRead(t, head).ContractShapesContext(ctx, nil)
	var pending *ErrStagePending
	require.ErrorAs(t, err, &pending)
	require.Equal(t, &ErrStagePending{Need: StageContracts, Published: StageDerived}, pending)
}

func TestSnapshotContractBatchLimits(t *testing.T) {
	ctx := t.Context()
	worker, head, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	s := stageRead(t, head)
	ids := make([]string, graph.ContractProjectionRowLimit+1)
	shapes, err := s.ContractShapesContext(ctx, ids)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, shapes)
	nodes, err := s.FullNodesByIDsContext(ctx, ids)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, nodes)
	require.NoError(t, s.ReadError())
}
