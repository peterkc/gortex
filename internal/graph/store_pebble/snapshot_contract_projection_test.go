package store_pebble

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestSnapshotContractProjectionMatchesNative(t *testing.T) {
	ctx := t.Context()
	worker, head, files, owner, raw, _, _ := relationsStageFixture(t, FullProfile)
	const repo, recorded = "old", "old/recorded.go"
	other := &graph.Node{ID: "old/other.go::Client", FilePath: "old/other.go", RepoPrefix: repo, Kind: graph.KindFunction, Language: "go"}
	goFrame := &graph.Node{ID: "route::goframe::GET::/g", FilePath: owner.FilePath, RepoPrefix: repo, Kind: graph.KindContract, Language: "go",
		Meta: map[string]any{"type": "http", "role": "provider", "framework": "goframe", "method": "GET", "path": "/g"}}
	shapeNode := &graph.Node{ID: "old/shape.go::Response", Kind: graph.KindType, FilePath: "old/shape.go", RepoPrefix: repo,
		Language: "go", StartLine: 1, EndLine: 4, Meta: map[string]any{"ordinary": true}}
	sqlSource := &graph.Node{ID: "query::source", FilePath: "other/query.sql", RepoPrefix: "other", Kind: graph.KindFunction}
	sqlTarget := &graph.Node{ID: "query::table", FilePath: "other/schema.sql", RepoPrefix: "other", Kind: graph.KindType}
	sqlEdge := &graph.Edge{From: sqlSource.ID, To: sqlTarget.ID, Kind: graph.EdgeProvides, FilePath: sqlSource.FilePath, Line: 3}
	mirror := &graph.Edge{From: owner.ID, To: goFrame.ID, Kind: graph.EdgeHandlesRoute, FilePath: owner.FilePath, Line: 3}
	worker.AddBatch([]*graph.Node{other, goFrame, shapeNode, sqlSource, sqlTarget}, []*graph.Edge{sqlEdge, mirror})
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	pinned := stageRead(t, head)

	winner := &graph.Node{ID: "http::GET::/shared", Name: "GET /shared", FilePath: owner.FilePath, RepoPrefix: repo, Kind: graph.KindContract, Language: "contract",
		Meta: map[string]any{"type": "http", "role": "provider", "contract_owner_record": true}}
	ownerless := &graph.Node{ID: "env::IDENTITY", FilePath: owner.FilePath, RepoPrefix: repo, Kind: graph.KindContract, Language: "contract",
		Meta: map[string]any{"type": "env", "role": "consumer", "contract_meta": map[string]any{"var": "IDENTITY"}}}
	provider := &graph.Edge{From: owner.ID, To: winner.ID, Kind: graph.EdgeProvides, FilePath: recorded, Line: 11,
		Meta: map[string]any{"contract_owner_symbol_id": owner.ID, "contract_owner_repo_prefix": repo, "contract_owner_workspace": "w", "contract_owner_project": "p",
			"contract_owner_confidence": 0.75, "contract_owner_meta": map[string]any{"path_param_names": []string{"project", "item"}, "path": "/shared"}}}
	consumer := &graph.Edge{From: other.ID, To: winner.ID, Kind: graph.EdgeConsumes, FilePath: other.FilePath, Line: 12,
		Meta: map[string]any{"contract_owner_symbol_id": other.ID, "contract_owner_repo_prefix": repo, "contract_owner_meta": nil}}
	route := &graph.Edge{From: owner.ID, To: winner.ID, Kind: graph.EdgeHandlesRoute, FilePath: recorded, Line: 11}
	shape := contracts.ExtractShape(shapeNode.FilePath, []byte("type Response struct {\n\tZ string `json:\"z\"`\n\tA []int `json:\"a,omitempty\"`\n}\n"), shapeNode.StartLine, shapeNode.EndLine)
	require.NotNil(t, shape)
	require.Len(t, shape.Fields, 2)
	omitted := RebuildContractOmission{RepoPrefix: repo, FilePath: owner.FilePath, Type: contracts.ContractHTTP, ID: "http::GET::/maybe", RecordKey: HeadDigest{9}}
	batch := RebuildContractBatch{
		Nodes:   []*graph.Node{winner, ownerless},
		Edges:   []*graph.Edge{provider, consumer, route},
		Shapes:  map[string]*contracts.Shape{shapeNode.ID: shape},
		Omitted: []RebuildContractOmission{omitted},
	}
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(batch), ContractCoverage{OmittedRecords: 1})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	current := stageRead(t, head)

	nativeShapeNode := *shapeNode
	// snapshotContractShapes assigns the extractor's *Shape directly.
	nativeShapeNode.Meta = map[string]any{"ordinary": true, "shape": shape}
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked(
		[]*graph.Node{owner, other, goFrame, &nativeShapeNode, sqlSource, sqlTarget, winner, ownerless},
		[]*graph.Edge{raw, mirror, sqlEdge, provider, consumer, route},
	))

	full, err := current.FullNodesByIDsContext(ctx, []string{shapeNode.ID})
	require.NoError(t, err)
	nativeSaved := native.GetNode(shapeNode.ID)
	require.NotNil(t, nativeSaved)
	require.IsType(t, (*contracts.Shape)(nil), nativeSaved.Meta["shape"])
	require.IsType(t, (*contracts.Shape)(nil), full[shapeNode.ID].Meta["shape"])
	require.Equal(t, nativeShapeNode.Meta, nativeSaved.Meta)
	require.Equal(t, nativeSaved.Meta, full[shapeNode.ID].Meta)
	require.Equal(t, shapeNode, current.GetNode(shapeNode.ID))
	require.Equal(t, pinned.GetNode(shapeNode.ID), current.GetNode(shapeNode.ID))
	wantLayer, err := native.LayerContractIDProjectionContext(ctx, []string{shapeNode.ID})
	require.NoError(t, err)
	gotLayer, err := current.LayerContractIDProjectionContext(ctx, []string{shapeNode.ID})
	require.NoError(t, err)
	require.Equal(t, nativeSaved, wantLayer.SourceNodes[shapeNode.ID])
	assertSnapshotContractProjection(t, wantLayer, gotLayer)

	for _, tc := range []struct {
		name string
		load func(graph.ContractFileProjectionReader, graph.ContractRepoProjectionReader) (graph.ContractFileProjection, error)
	}{
		{"repo", func(_ graph.ContractFileProjectionReader, r graph.ContractRepoProjectionReader) (graph.ContractFileProjection, error) {
			return r.LoadContractRepoProjectionContext(ctx, repo)
		}},
		{"files and off-file owner", func(r graph.ContractFileProjectionReader, _ graph.ContractRepoProjectionReader) (graph.ContractFileProjection, error) {
			return r.LoadContractFileProjectionContext(ctx, repo, []string{owner.FilePath, shapeNode.FilePath})
		}},
		{"recording file without nodes", func(r graph.ContractFileProjectionReader, _ graph.ContractRepoProjectionReader) (graph.ContractFileProjection, error) {
			return r.LoadContractFileProjectionContext(ctx, repo, []string{recorded})
		}},
		{"IDs and shared SQL kind", func(r graph.ContractFileProjectionReader, _ graph.ContractRepoProjectionReader) (graph.ContractFileProjection, error) {
			return r.LoadContractIDProjectionContext(ctx, []string{winner.ID, sqlSource.ID, sqlTarget.ID, shapeNode.ID})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := tc.load(native, native)
			require.NoError(t, err)
			got, err := tc.load(current, current)
			require.NoError(t, err)
			assertSnapshotContractProjection(t, want, got)
			if tc.name == "files and off-file owner" {
				require.Contains(t, got.OffFileOwnerRows, graph.RepoEdgeRow{RepoPrefix: repo, Edge: provider})
			}
			if tc.name == "IDs and shared SQL kind" {
				require.Contains(t, got.OwnerRows, graph.RepoEdgeRow{RepoPrefix: "other", Edge: sqlEdge})
				require.NotContains(t, got.Targets, sqlTarget.ID)
			}
		})
	}

	for _, opts := range []contracts.RegistryLoadOptions{
		{RepoPrefix: repo},
		{RepoPrefix: repo, FilePaths: []string{recorded}},
		{RepoPrefix: repo, FilePaths: []string{recorded}, ExpandFileIDs: true},
		{RepoPrefix: repo, ContractIDs: []string{winner.ID}},
		{RepoPrefix: repo, ContractIDs: []string{goFrame.ID}},
	} {
		want, err := contracts.LoadRegistryFromGraphChecked(ctx, native, opts)
		require.NoError(t, err)
		require.NotNil(t, want)
		got, err := contracts.LoadRegistryFromGraphChecked(ctx, current, opts)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.ElementsMatch(t, want.All(), got.All())
		if len(opts.ContractIDs) == 1 && opts.ContractIDs[0] == goFrame.ID {
			require.Len(t, want.ByID(goFrame.ID), 1, "native retains the GoFrame scalar beside its HandlesRoute edge")
		}
	}
	_, err = contracts.LoadRegistryFromGraphChecked(ctx, pinned, contracts.RegistryLoadOptions{RepoPrefix: repo})
	require.ErrorAs(t, err, new(*ErrStagePending))

	for _, id := range []string{winner.ID, omitted.ID, "http::GET::/absent"} {
		found, err := contracts.LoadRegistryFromGraphChecked(ctx, current, contracts.RegistryLoadOptions{RepoPrefix: repo, ContractIDs: []string{id}})
		require.NoError(t, err)
		scope := contracts.ReadScope{RepoPrefixes: []string{repo}, Types: []contracts.ContractType{contracts.ContractHTTP}, ContractIDs: []string{id}}
		partial, err := current.ContractCoverageContext(ctx, scope, found == nil)
		if id == omitted.ID {
			require.Nil(t, found)
			require.True(t, partial)
			require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
		} else {
			require.Equal(t, id == winner.ID, found != nil)
			require.False(t, partial)
			require.NoError(t, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = current.LoadContractIDProjectionContext(canceled, []string{winner.ID})
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, current.ReadError())
	limited, err := current.readContractProjection(ctx, func(ctx context.Context, p *graph.ContractFileProjection, budget *int) error {
		*budget = 1 // this file has more than one retained row
		return current.contractFileNodes(ctx, p, budget, repo, owner.FilePath)
	})
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Zero(t, limited)
	require.NoError(t, current.ReadError())

	reads := trackSnapshotReads(current)
	reads.failPoint = string(pendingNodeKey(owner.ID))
	reads.failure = errors.New("injected contract owner source read failure")
	got, err := contracts.LoadRegistryFromGraphChecked(ctx, current, contracts.RegistryLoadOptions{RepoPrefix: repo, ContractIDs: []string{winner.ID}})
	require.ErrorIs(t, err, reads.failure)
	require.ErrorIs(t, current.ReadError(), reads.failure)
	require.Nil(t, got)

	require.NoError(t, head.db.Delete(pendingNodeKey(owner.ID), pebble.Sync))
	broken := stageRead(t, head)
	got, err = contracts.LoadRegistryFromGraphChecked(ctx, broken, contracts.RegistryLoadOptions{RepoPrefix: repo, ContractIDs: []string{winner.ID}})
	require.ErrorIs(t, err, graph.ErrContractProjectionIncomplete)
	require.Nil(t, got)
}

func assertSnapshotContractProjection(t *testing.T, want, got graph.ContractFileProjection) {
	t.Helper()
	require.Len(t, got.FileNodes, len(want.FileNodes))
	for path, nodes := range want.FileNodes {
		require.ElementsMatch(t, nodes, got.FileNodes[path], path)
	}
	require.ElementsMatch(t, want.ScalarNodes, got.ScalarNodes)
	require.Equal(t, want.Targets, got.Targets)
	require.ElementsMatch(t, want.OwnerRows, got.OwnerRows)
	require.ElementsMatch(t, want.OffFileOwnerRows, got.OffFileOwnerRows)
	require.Equal(t, want.SourceNodes, got.SourceNodes)
	require.ElementsMatch(t, want.OutgoingOwnerRows, got.OutgoingOwnerRows)
}

func TestSnapshotContractProjectionBuiltEmpty(t *testing.T) {
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
	got, err := contracts.LoadRegistryFromGraphChecked(ctx, current, contracts.RegistryLoadOptions{RepoPrefix: "old"})
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSnapshotContractProjectionEmptyPathAndExplicitOwnerRepo(t *testing.T) {
	ctx := t.Context()
	worker, head, files, owner, _, _, _ := relationsStageFixture(t, FullProfile)
	foreign := &graph.Node{ID: "env::FOREIGN", RepoPrefix: "other", Kind: graph.KindContract, Language: "contract"}
	edge := &graph.Edge{From: owner.ID, To: foreign.ID, Kind: graph.EdgeProvides, Meta: map[string]any{"contract_owner_repo_prefix": "other"}}
	worker.AddBatch([]*graph.Node{foreign}, []*graph.Edge{edge})
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
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked([]*graph.Node{owner, foreign}, []*graph.Edge{edge}))
	want, err := native.LayerContractFileProjectionContext(ctx, "old", []string{""})
	require.NoError(t, err)
	got, err := s.LayerContractFileProjectionContext(ctx, "old", []string{""})
	require.NoError(t, err)
	assertSnapshotContractProjection(t, want, got)
	require.Empty(t, got.FileNodes)
	require.Empty(t, got.OwnerRows)
	want, err = native.LayerContractRepoProjectionContext(ctx, "old")
	require.NoError(t, err)
	got, err = s.LayerContractRepoProjectionContext(ctx, "old")
	require.NoError(t, err)
	assertSnapshotContractProjection(t, want, got)
	require.Empty(t, got.OwnerRows)
}

func TestSnapshotContractProjectionRejectsMalformedIndexes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"node ID", "contract projection node differs from its key"},
		{"owner source ID", "contract projection node differs from its key"},
		{"repo counter", "contract projection repository counter key"},
		{"file index", "contract projection file index differs from its node"},
		{"incoming index", "contract projection adjacency differs from its edge"},
		{"outgoing index", "contract projection adjacency differs from its edge"},
		{"node kind index", "contract projection kind index differs from its node"},
		{"edge kind missing", "contract projection kind index missing its edge"},
		{"edge kind identity", "contract projection kind index differs from its edge"},
		{"full node ID", "contract projection node differs from its key"},
		{"legacy shape", "shape must come from the split contract family"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			worker, head, files, owner, _, _, _ := relationsStageFixture(t, FullProfile)
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
			var key, value []byte
			e := &graph.Edge{From: owner.ID, To: owner.ID, Kind: graph.EdgeProvides, FilePath: owner.FilePath, Line: 1}
			id := edgeIdentity(e)
			edgeBytes, err := encodeEdge(e)
			require.NoError(t, err)
			switch tc.name {
			case "node ID", "full node ID":
				key = pendingNodeKey("forged")
				value, err = encodeNode(owner)
			case "owner source ID":
				e.From = "forged"
				id := edgeIdentity(e)
				out := outEdgeKey(e, id)
				encoded, err := encodeEdge(e)
				require.NoError(t, err)
				require.NoError(t, head.db.Set(out, encoded, pebble.Sync))
				kind := append(appendKeyLine(keyStrings([]byte{familyEdge, edgeKind}, string(e.Kind), e.From), e.Line), id[:]...)
				require.NoError(t, head.db.Set(kind, out, pebble.Sync))
				key = pendingNodeKey(e.From)
				value, err = encodeNode(owner)
				require.NoError(t, err)
			case "repo counter":
				key = append(countKey("repo"), 0)
				value = encodeCount(1)
			case "file index":
				key = keyStrings([]byte{familyFile}, owner.RepoPrefix, owner.FilePath, "forged")
				value = []byte(owner.ID)
			case "incoming index":
				key = append(keyStrings([]byte{familyEdge, edgeIn}, owner.ID, string(e.Kind)), id[:]...)
				key[len(key)-1] ^= 1
				value = edgeBytes
			case "outgoing index":
				key = append(appendKeyLine(keyStrings([]byte{familyEdge, edgeOutKind}, owner.ID, string(e.Kind)), e.Line), id[:]...)
				key[len(key)-1] ^= 1
				value = edgeBytes
			case "node kind index":
				key = keyStrings([]byte{familyNodeKind}, string(graph.KindContract), owner.ID)
				value = []byte(owner.ID)
			case "edge kind missing", "edge kind identity":
				key = append(appendKeyLine(keyStrings([]byte{familyEdge, edgeKind}, string(e.Kind), e.From), e.Line), id[:]...)
				value = outEdgeKey(e, id)
				if tc.name == "edge kind identity" {
					key[len(key)-1] ^= 1
					require.NoError(t, head.db.Set(value, edgeBytes, pebble.Sync))
				}
			case "legacy shape":
				key = pendingNodeKey(owner.ID)
				bad := *owner
				bad.Kind = graph.KindType
				bad.Meta = map[string]any{"shape": &contracts.Shape{Kind: "struct"}}
				value, err = encodeNode(&bad)
			}
			require.NoError(t, err)
			require.NoError(t, head.db.Set(key, value, pebble.Sync))
			s := stageRead(t, head)
			var p graph.ContractFileProjection
			switch tc.name {
			case "node ID":
				p, err = s.LayerContractIDProjectionContext(ctx, []string{"forged"})
			case "full node ID", "legacy shape":
				id := owner.ID
				if tc.name == "full node ID" {
					id = "forged"
				}
				var nodes map[string]*graph.Node
				nodes, err = s.FullNodesByIDsContext(ctx, []string{id})
				require.Nil(t, nodes)
			case "repo counter", "file index":
				p, err = s.LayerContractFileProjectionContext(ctx, owner.RepoPrefix, []string{owner.FilePath})
			case "incoming index", "outgoing index":
				p, err = s.LayerContractIDProjectionContext(ctx, []string{owner.ID})
			default:
				p, err = s.LayerContractRepoProjectionContext(ctx, owner.RepoPrefix)
			}
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tc.want)
			require.Zero(t, p)
			require.ErrorContains(t, s.ReadError(), tc.want)
		})
	}
}
