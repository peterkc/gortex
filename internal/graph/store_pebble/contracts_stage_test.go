package store_pebble

import (
	"bytes"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func contractBatches(batches ...RebuildContractBatch) iter.Seq2[RebuildContractBatch, error] {
	return func(yield func(RebuildContractBatch, error) bool) {
		for _, batch := range batches {
			if !yield(batch, nil) {
				return
			}
		}
	}
}

func testContractRow() *graph.Node {
	return &graph.Node{ID: "http::GET::/a", Kind: graph.KindContract, Name: "http::GET::/a", Language: "contract", FilePath: "old.go",
		Meta: map[string]any{"type": "http", "role": "provider", "contract_owner_record": true, "contract_meta": map[string]any{
			"status_codes": []int{200, 404}, "empty": []int{}, "nil": []int(nil), "absent": nil,
			"owner": map[string]string{"name": "a"},
		}}}
}

func testContractOmission() RebuildContractOmission {
	return RebuildContractOmission{RepoPrefix: "old", FilePath: "old.go", Type: contracts.ContractHTTP, ID: "http::GET::/deferred", RecordKey: HeadDigest{1}}
}

func TestContractsStagePublishesExactDiff(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := t.Context()
			worker, head, files, declared, raw, _, _ := relationsStageFixture(t, profile)
			// Change the FileGraph node's secondary keys during Relations, so
			// the retained predecessor has both new rows and tombstones.
			owner := *declared
			owner.Kind, owner.Name = graph.KindFunction, "Handler"
			worker.AddNode(&owner)
			shapeNode := &graph.Node{ID: "old.go::T", Kind: graph.KindType, Name: "T", FilePath: "old.go", RepoPrefix: "old", Language: "go", Meta: map[string]any{"ordinary": true}}
			local := &graph.Node{ID: "old.go::local", Kind: graph.KindLocal, Name: "local", FilePath: "old.go", RepoPrefix: "old"}
			seed := &graph.Node{ID: "dep::example.org/pkg", Kind: graph.KindContract, Name: "dep::example.org/pkg", FilePath: "go.mod", Language: "contract",
				Meta: map[string]any{"type": "dependency", "role": "consumer"}}
			parseContract := &graph.Node{ID: "route::goframe::GET::/a", Kind: graph.KindContract, Name: "GET /a", Language: "go", FilePath: "old.go"}
			imp := &graph.Edge{From: owner.ID, To: seed.ID, Kind: graph.EdgeImports, FilePath: owner.FilePath, Line: 1}
			worker.AddBatch([]*graph.Node{shapeNode, local, seed, parseContract}, []*graph.Edge{imp})
			goReturn := &graph.Edge{From: owner.ID, To: "external::error", Kind: graph.EdgeReturns, FilePath: owner.FilePath, Line: 2, Origin: graph.OriginASTInferred}
			worker.AddEdge(goReturn)
			require.NoError(t, worker.recordExternalReceipt(ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: owner.FilePath}}, goReturn))
			relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			base, err := worker.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			relationDigest, err := head.PublishStage(ctx, relations)
			require.NoError(t, err)
			pinned := stageRead(t, head)
			oldRows := relationsSavedRows(t, pinned)
			oldShapeNode, err := point(pinned, pendingNodeKey(shapeNode.ID), copyValue)
			require.NoError(t, err)
			beforeBytes := pinned.RepoMemoryEstimate("old").NodeBytes
			shape := &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "Z", Type: "int", Required: true}, {Name: "A", Type: "[]string", Repeated: true}}, Notes: []string{}}
			contract := testContractRow()
			completedSeed := *seed
			completedSeed.Meta = map[string]any{"type": "dependency", "role": "consumer", "contract_meta": map[string]any{"version": "v1.2.3"}}
			provided := &graph.Edge{From: owner.ID, To: contract.ID, Kind: graph.EdgeProvides, FilePath: owner.FilePath, Line: 3, Meta: map[string]any{"contract_meta": map[string]any{"status_codes": []int{200, 404}}}}
			route := &graph.Edge{From: owner.ID, To: contract.ID, Kind: graph.EdgeHandlesRoute, FilePath: owner.FilePath, Line: 3}
			omitted := &graph.Edge{From: local.ID, To: contract.ID, Kind: graph.EdgeConsumes, FilePath: owner.FilePath, Line: 4}
			omission := testContractOmission()
			tables, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(
				RebuildContractBatch{Edges: []*graph.Edge{provided, route, omitted}, Omitted: []RebuildContractOmission{omission}},
				RebuildContractBatch{Nodes: []*graph.Node{contract, &completedSeed}, Shapes: map[string]*contracts.Shape{shapeNode.ID: shape}},
			), ContractCoverage{OmittedRecords: 1})
			require.NoError(t, err)
			require.Equal(t, relationDigest, tables.Base)
			require.Equal(t, StageContracts, tables.Witness.Stage)
			var ids []string
			require.NoError(t, worker.VisitPreparedContractIDs(ctx, tables, func(id string) error { ids = append(ids, id); return nil }))
			require.Equal(t, []string{seed.ID, contract.ID}, ids)
			bad := tables
			bad.Base = HeadDigest{99}
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, ErrStaleBase)
			bad = tables
			bad.Incarnation = testReplacementIncarnation
			_, err = head.PublishStage(ctx, bad)
			require.ErrorIs(t, err, ErrStaleBase)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.ErrorIs(t, err, ErrStageOrder)
			current := stageRead(t, head)
			key, err := contractOmissionKey(omission)
			require.NoError(t, err)
			value, err := point(current, key, copyValue)
			require.NoError(t, err)
			record, err := decodeContractOmission(key, value)
			require.NoError(t, err)
			require.Equal(t, omission, record)
			oldValue, err := point(pinned, key, copyValue)
			require.NoError(t, err)
			require.Nil(t, oldValue)
			require.Equal(t, contract, current.GetNode(contract.ID))
			require.Equal(t, &completedSeed, current.GetNode(seed.ID))
			require.Equal(t, []*graph.Edge{imp}, current.GetInEdges(seed.ID))
			require.Equal(t, 1, current.count("external import endpoints"))
			require.Contains(t, current.GetOutEdges(owner.ID), raw)
			require.Contains(t, current.GetInEdges(contract.ID), provided)
			require.Contains(t, current.GetInEdges(contract.ID), route)
			require.Equal(t, []*graph.Node{contract}, current.FindNodesByName(contract.Name))
			if profile == FullProfile {
				require.Contains(t, current.GetInEdges(contract.ID), omitted)
				require.Equal(t, 3, current.count("in total", contract.ID))
			} else {
				require.NotContains(t, current.GetInEdges(contract.ID), omitted)
				require.Equal(t, 2, current.count("in total", contract.ID))
			}
			savedShape, err := point(current, contractShapeKey(shapeNode.ID), decodeContractShape)
			require.NoError(t, err)
			require.Equal(t, shape, savedShape)
			newShapeNode, err := point(current, pendingNodeKey(shapeNode.ID), copyValue)
			require.NoError(t, err)
			require.Equal(t, oldShapeNode, newShapeNode)
			shapeBytes, err := encodeContractShape(shape)
			require.NoError(t, err)
			require.Equal(t, beforeBytes+uint64(len(shapeBytes)), current.RepoMemoryEstimate("old").NodeBytes)
			require.Equal(t, pinned.NodeCount()+1, current.NodeCount())
			require.Equal(t, tables.Witness.Families, relationsSavedWitness(t, current))
			require.NoError(t, current.ReadError())
			require.Equal(t, oldRows, relationsSavedRows(t, pinned))
			require.Nil(t, pinned.GetNode(contract.ID))
			require.Equal(t, seed, pinned.GetNode(seed.ID))
			require.ErrorAs(t, pinned.RequireStage(StageContracts), new(*ErrStagePending))
			oldShape, err := point(pinned, contractShapeKey(shapeNode.ID), decodeContractShape)
			require.NoError(t, err)
			require.Nil(t, oldShape)
		})
	}
}

func TestContractsStageRefusals(t *testing.T) {
	for _, mode := range []string{"duplicate node", "duplicate edge", "duplicate shape", "duplicate omission", "missing omission", "uncounted omission", "collision", "missing endpoint", "missing shape type", "stale predecessor", "corrupt predecessor", "unclaimed node change", "unclaimed edge change", "empty", "only deferred"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			worker, head, files, declared, _, _, _ := relationsStageFixture(t, FullProfile)
			owner := *declared
			owner.Kind = graph.KindType
			worker.AddNode(&owner)
			relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			if mode == "stale predecessor" {
				relations.Base = HeadDigest{9}
				_, err := worker.RetainRelations(ctx, files, relations)
				require.ErrorIs(t, err, ErrStaleBase)
				return
			}
			base, err := worker.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			pinned := stageRead(t, head)
			oldRows := relationsSavedRows(t, pinned)
			contract := testContractRow()
			e := &graph.Edge{From: owner.ID, To: contract.ID, Kind: graph.EdgeProvides, FilePath: owner.FilePath}
			batch := RebuildContractBatch{Nodes: []*graph.Node{contract}, Edges: []*graph.Edge{e}}
			shape := &contracts.Shape{Kind: "struct"}
			var again RebuildContractBatch
			var coverage ContractCoverage
			switch mode {
			case "duplicate node":
				again.Nodes = []*graph.Node{contract}
			case "duplicate edge":
				again.Edges = []*graph.Edge{e}
			case "duplicate shape":
				batch.Shapes = map[string]*contracts.Shape{owner.ID: shape}
				again.Shapes = batch.Shapes
			case "duplicate omission":
				batch.Omitted = []RebuildContractOmission{testContractOmission()}
				again.Omitted = batch.Omitted
				coverage.OmittedRecords = 2
			case "missing omission":
				coverage.OmittedRecords = 1
			case "uncounted omission":
				batch.Omitted = []RebuildContractOmission{testContractOmission()}
			case "collision":
				contract.ID = owner.ID
			case "missing endpoint":
				e.To = "external::unproved"
			case "missing shape type":
				batch.Shapes = map[string]*contracts.Shape{"absent": shape}
			case "unclaimed node change":
				worker.AddNode(&graph.Node{ID: owner.ID, Name: "changed", Kind: graph.KindType})
			case "unclaimed edge change":
				worker.AddEdge(&graph.Edge{From: owner.ID, To: owner.ID, Kind: graph.EdgeImports})
			case "corrupt predecessor":
				require.NotEmpty(t, base.paths)
				require.NoError(t, os.Remove(base.paths[0]))
			case "empty", "only deferred":
				batch = RebuildContractBatch{}
				if mode == "only deferred" {
					a := testContractOmission()
					b := a
					b.RecordKey = HeadDigest{2}
					batch.Omitted = []RebuildContractOmission{a, b}
					coverage.OmittedRecords = 2
				}
			}
			stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(batch, again), coverage)
			if mode == "empty" || mode == "only deferred" {
				require.NoError(t, err)
				if mode == "empty" {
					require.Empty(t, stage.Files)
				} else {
					require.NotEmpty(t, stage.Files)
				}
				require.Equal(t, &coverage, stage.Witness.Contracts)
				_, err = head.PublishStage(ctx, stage)
				require.NoError(t, err)
				current := stageRead(t, head)
				require.NoError(t, current.RequireStage(StageContracts))
				require.Equal(t, pinned.NodeCount(), current.NodeCount())
				require.Equal(t, stage.Witness.Families, relationsSavedWitness(t, current))
				return
			}
			require.ErrorIs(t, err, ErrContractsSeal)
			if mode == "collision" {
				var collision *ContractIDCollisionError
				require.ErrorAs(t, err, &collision)
				require.Equal(t, owner.ID, collision.ID)
				require.Equal(t, 1, collision.Count)
			}
			require.Empty(t, stage)
			require.Equal(t, oldRows, relationsSavedRows(t, stageRead(t, head)))
			require.ErrorAs(t, stageRead(t, head).RequireStage(StageContracts), new(*ErrStagePending))
		})
	}
}

func TestContractShapeCodec(t *testing.T) {
	for _, fields := range [][]contracts.ShapeField{nil, {}, {{Name: "A", Type: "int", Comment: "wire", JSONTag: "a", Required: true, Repeated: true}}} {
		shape := &contracts.Shape{Kind: "struct", Fields: fields, Notes: []string{"note"}}
		raw, err := encodeContractShape(shape)
		require.NoError(t, err)
		got, err := decodeContractShape(raw)
		require.NoError(t, err)
		require.Equal(t, shape, got)
		for n := range len(raw) {
			_, err := decodeContractShape(raw[:n])
			require.Error(t, err)
		}
		_, err = decodeContractShape(append(bytes.Clone(raw), 0))
		require.Error(t, err)
	}
	require.NotEqual(t, contractShapeKey("a\x00b"), contractShapeKey("a"))
}

func TestContractOmissionCodec(t *testing.T) {
	o := testContractOmission()
	for _, id := range []string{"", "http::GET::/a\x00b"} {
		o.ID = id
		key, err := contractOmissionKey(o)
		require.NoError(t, err)
		value := []byte{contractOmissionFormat}
		got, err := decodeContractOmission(key, value)
		require.NoError(t, err)
		require.Equal(t, o, got)
		for n := range len(key) {
			_, err := decodeContractOmission(key[:n], value)
			require.ErrorIs(t, err, errInvalidRecord)
		}
		_, err = decodeContractOmission(key, []byte{0})
		require.ErrorIs(t, err, errInvalidRecord)
	}
	o.FilePath = strings.Repeat("a", maxContractOmissionKeyBytes)
	_, err := contractOmissionKey(o)
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestContractsStageOmissionScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RebuildContractOmission)
		want   string
	}{
		{"unadmitted file", func(o *RebuildContractOmission) { o.FilePath = "not-admitted.py" }, "omission lacks admitted file scope"},
		{"foreign repo", func(o *RebuildContractOmission) { o.RepoPrefix = "other" }, "omission lacks admitted file scope"},
		{"empty scope", func(o *RebuildContractOmission) { o.FilePath = "" }, "omission scope"},
		{"unknown type", func(o *RebuildContractOmission) {
			o.Type = "invented"
			o.ID = ""
		}, "omission type or ID"},
		{"mismatched ID", func(o *RebuildContractOmission) { o.ID = "env::KNOWN" }, "omission type or ID"},
		{"OpenAPI alias", func(o *RebuildContractOmission) { o.Type = contracts.ContractOpenAPI }, ""},
		{"dependency alias", func(o *RebuildContractOmission) {
			o.Type = contracts.ContractDependency
			o.ID = "dep::example.org/pkg"
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			worker, head, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
			relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			base, err := worker.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			omitted := testContractOmission()
			tc.change(&omitted)
			batch := RebuildContractBatch{Omitted: []RebuildContractOmission{omitted}}
			stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(batch), ContractCoverage{OmittedRecords: 1})
			if tc.want == "" {
				require.NoError(t, err)
				require.Equal(t, uint64(1), stage.Witness.Contracts.OmittedRecords)
				return
			}
			require.ErrorIs(t, err, ErrContractsSeal)
			require.ErrorContains(t, err, tc.want)
			require.Zero(t, stage)
			require.ErrorAs(t, stageRead(t, head).RequireStage(StageContracts), new(*ErrStagePending))
		})
	}
}

func TestContractOmissionNativeIDTypes(t *testing.T) {
	for _, record := range []RebuildContractOmission{
		{Type: contracts.ContractHTTP, ID: "http::GET::/users"},
		{Type: contracts.ContractGRPC, ID: "grpc::UserService::GetUser"},
		{Type: contracts.ContractThrift, ID: "thrift::Calculator::ping"},
		{Type: contracts.ContractGraphQL, ID: "graphql::Query::user"},
		{Type: contracts.ContractTopic, ID: "topic::kafka::orders"},
		{Type: contracts.ContractWS, ID: "ws::message"},
		{Type: contracts.ContractEnv, ID: "env::DATABASE_URL"},
		{Type: contracts.ContractOpenAPI, ID: "http::GET::/pets"},
		{Type: contracts.ContractDependency, ID: "dep::example.org/pkg"},
		{Type: contracts.ContractDI, ID: "di::Notifier"},
		{Type: contracts.ContractTRPC, ID: "trpc::userRouter.getUser"},
		{Type: contracts.ContractType(graph.EdgeReadsConfig), ID: "cfg::spring::w::r::server.port"},
	} {
		require.True(t, contractOmissionIDMatchesType(record), "%s: %s", record.Type, record.ID)
	}
	for _, id := range []string{"", "cfg::spring::", "env::MISSING"} {
		require.False(t, contractOmissionIDMatchesType(RebuildContractOmission{Type: contracts.ContractType(graph.EdgeReadsConfig), ID: id}))
	}
}

func TestContractStageRejectsInvalidBatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch RebuildContractBatch
		want  string
	}{
		{"invalid node", RebuildContractBatch{Nodes: []*graph.Node{{ID: "ordinary", Kind: graph.KindType}}}, "invalid contract stage node"},
		{"wrong registry language", RebuildContractBatch{Nodes: []*graph.Node{{ID: "http::GET::/a", Kind: graph.KindContract, Language: "go"}}}, "contract registry node language"},
		{"unsplit shape", RebuildContractBatch{Nodes: []*graph.Node{{ID: "env::A", Kind: graph.KindContract, Language: "contract", Meta: map[string]any{"shape": nil}}}}, "contract batch must supply split shape rows"},
		{"invalid edge", RebuildContractBatch{Edges: []*graph.Edge{{From: "", To: "env::A", Kind: graph.EdgeProvides}}}, "invalid contract stage edge"},
		{"unproved Call", RebuildContractBatch{Edges: []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeCalls}}}, "unproved contract Call emitter"},
		{"unexpected edge kind", RebuildContractBatch{Edges: []*graph.Edge{{From: "a", To: "b", Kind: graph.EdgeReferences}}}, "unexpected contract stage edge kind"},
		{"empty type ID", RebuildContractBatch{Shapes: map[string]*contracts.Shape{"": {Kind: "struct"}}}, "empty contract shape ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			err := b.writeContractBatch(t.Context(), tc.batch)
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestContractStageAttemptAndIDVisit(t *testing.T) {
	ctx := t.Context()
	b, _, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	relations, err := b.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := b.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	unsealed := relations
	unsealed.Witness.Stage = StageContracts
	unsealed.Witness.Contracts = &ContractCoverage{}
	visit := func(string) error {
		t.Fatal("unsealed ID visit reached its callback")
		return nil
	}
	require.ErrorContains(t, b.VisitPreparedContractIDs(ctx, unsealed, visit), "does not name the prepared stage")
	_, err = b.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
	require.NoError(t, err)
	stage, err := b.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
	require.ErrorContains(t, err, "contracts already attempted for this build")
	require.Zero(t, stage)
}

func TestContractsStageRejectsUnclaimedPrivateRows(t *testing.T) {
	for _, tag := range []byte{familyShape, familyOmission} {
		t.Run(string(tag), func(t *testing.T) {
			ctx := t.Context()
			b, _, files, owner, _, _, _ := relationsStageFixture(t, FullProfile)
			owner.Kind = graph.KindInterface
			b.AddNode(owner)
			relations, err := b.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			base, err := b.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			var coverage ContractCoverage
			if tag == familyShape {
				raw, err := encodeContractShape(&contracts.Shape{Kind: "interface"})
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(contractShapeKey(owner.ID), raw, pebble.NoSync))
			} else {
				key, err := contractOmissionKey(testContractOmission())
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, []byte{contractOmissionFormat}, pebble.NoSync))
				coverage.OmittedRecords = 1
			}
			stage, err := b.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), coverage)
			require.ErrorContains(t, err, "unclaimed primary row changed after relations")
			require.Zero(t, stage)
		})
	}
}

func TestContractsStageRetainsIncumbentEdge(t *testing.T) {
	ctx := t.Context()
	worker, head, files, source, _, _, _ := relationsStageFixture(t, FullProfile)
	incumbent := &graph.Edge{From: source.ID, To: source.ID, Kind: graph.EdgeProvides, FilePath: source.FilePath, Line: 9,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"owner": "first"}}
	worker.AddEdge(incumbent)
	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	pinned := stageRead(t, head)
	loser := *incumbent
	loser.Origin = "later"
	loser.Meta = map[string]any{"owner": "second"}
	inserted := *incumbent
	inserted.Line++
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(RebuildContractBatch{
		Edges: []*graph.Edge{&loser, &inserted},
	}), ContractCoverage{})
	require.NoError(t, err)
	require.NotEmpty(t, stage.Files)
	for edge, want := range map[*graph.Edge]bool{incumbent: false, &inserted: true} {
		marked, err := pendingGet(worker.pending, contractBatchKey('e', pendingEdgeKey(edge)), func([]byte) (bool, error) { return true, nil })
		require.NoError(t, err)
		require.Equal(t, want, marked)
	}
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	current := stageRead(t, head)
	require.Contains(t, current.GetOutEdges(source.ID), incumbent)
	require.ElementsMatch(t, append(pinned.GetOutEdges(source.ID), &inserted), current.GetOutEdges(source.ID))
	require.Equal(t, pinned.EdgeIdentityRevisions(), current.EdgeIdentityRevisions())
}

func TestRetainRelationsRefusesWrongWitness(t *testing.T) {
	ctx := t.Context()
	b, _, files, _, _, _, _ := relationsStageFixture(t, FullProfile)
	relations, err := b.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	other := newBuild(t, FullProfile)
	_, err = other.RetainRelations(ctx, files, relations)
	require.ErrorContains(t, err, "needs retained relations from this build")
	wrong := relations
	wrong.Witness.Stage = StageFileGraph
	_, err = b.RetainRelations(ctx, files, wrong)
	require.ErrorContains(t, err, "needs retained relations from this build")
	for _, tag := range []byte{familyManifest, familyFileMeta} {
		wrong = relations
		wrong.Witness.Families = slices.Clone(relations.Witness.Families)
		for i := range wrong.Witness.Families {
			if wrong.Witness.Families[i].Family == tag {
				wrong.Witness.Families[i].Count++
			}
		}
		_, err = b.RetainRelations(ctx, files, wrong)
		require.ErrorContains(t, err, "relations changed an immutable family")
	}
}

func TestContractsStageRejectsForgedPredecessorRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
		want string
	}{
		{"absent deletion", pendingNodeKey("absent"), "retained relations deletes an absent row"},
		{"endpoint counter deletion", countKey("external import endpoints"), "invalid retained endpoint counter"},
		{"missing relations rows", nil, "retained relations rows differ from their witness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b, _, files, _, raw, _, _ := relationsStageFixture(t, FullProfile)
			// Keep the real Relations diff free of primary changes. Dropping
			// the raw Call from the retained tables but not from the build
			// would trip the unclaimed-edge check during the merge.
			require.Equal(t, 1, b.RemoveEdgesExact([]*graph.Edge{raw}))
			relations, err := b.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			base, err := b.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			revision := countKey("revisions") // present from Relations onward
			delta := fileGraphCursor{ctx: ctx, options: b.options, paths: base.paths, delta: true}
			t.Cleanup(func() { require.NoError(t, delta.Close()) })
			require.True(t, delta.First())
			require.Equal(t, revision, delta.Key())
			require.False(t, delta.deleted)
			require.False(t, delta.Next())
			require.NoError(t, delta.Error())
			if tc.key == nil {
				base.paths = nil // remove only the witnessed revision counter
			} else {
				path := filepath.Join(t.TempDir(), "forged.sst")
				w, err := newTable(path, b.options.MakeWriterOptions(goldLevel, b.db.FormatMajorVersion().MaxTableFormat()))
				require.NoError(t, err)
				require.NoError(t, w.Delete(tc.key))
				require.NoError(t, w.Close())
				if bytes.Compare(tc.key, revision) < 0 {
					base.paths = append([]string{path}, base.paths...)
				} else {
					base.paths = append(base.paths, path)
				}
			}
			stage, err := b.PrepareContracts(ctx, t.TempDir(), base, contractBatches(), ContractCoverage{})
			require.ErrorIs(t, err, ErrContractsSeal)
			require.ErrorContains(t, err, tc.want)
			require.Zero(t, stage)
		})
	}
}
