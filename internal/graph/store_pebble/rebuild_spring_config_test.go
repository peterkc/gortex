package store_pebble

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildSpringConfigSealsOnlyMissingTargetAsPartial(t *testing.T) {
	ctx := t.Context()
	worker, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	head, err := OpenHead(t.TempDir(), testHeadIncarnation, FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	scope := contracts.SpringConfigScope{RepoRoot: t.TempDir(), RepoPrefix: "r", WorkspaceID: "w"}
	const beanFile = "src/Bean.java"
	java, err := languages.NewJavaExtractor().Extract(beanFile, []byte(`package p;
@ConfigurationProperties(prefix = "app")
class Bean {
    @Value("${app.httpPort}") int present;
    @Value("${env.only:local}") String missing;
    @Value("${ENV.ON-LY:fallback}") String sameMissing;
}
`))
	require.NoError(t, err)
	t.Cleanup(java.ReleaseTree)
	var bean *graph.Node
	for _, n := range java.Nodes {
		if n.Kind == graph.KindType && n.Name == "Bean" {
			bean = n
			break
		}
	}
	require.NotNil(t, bean)
	require.ElementsMatch(t, []string{"app.*", "app.httpPort", "env.only", "ENV.ON-LY"}, contracts.SpringConfigKeysOf(bean))
	bean.RepoPrefix, bean.WorkspaceID = scope.RepoPrefix, scope.WorkspaceID
	const firstPath = "a/application-prod.properties"
	const laterPath = "z/application.properties"
	const foreignPath = "0/application.properties"
	config := map[string][]byte{
		firstPath:   []byte("app.http-port=8080\napp.deep.other=hidden\n"),
		laterPath:   []byte("app.httpPort=9999\napp.second=visible\n"),
		foreignPath: []byte("env.only=foreign\n"),
	}
	file := func(id, path, language, repo string) *graph.Node {
		return &graph.Node{ID: id, FilePath: path, Language: language, RepoPrefix: repo, WorkspaceID: scope.WorkspaceID, Kind: graph.KindFile}
	}
	// File node ID order is not projected path order.
	nodes := []*graph.Node{bean,
		file(beanFile, beanFile, "java", "r"),
		file("last-id", firstPath, "", "r"),
		file("first-id", laterPath, "", "r"),
		file("foreign", foreignPath, "", "other"),
	}
	native := graph.New()
	native.AddBatch(nodes, nil)
	var manifestFiles []ManifestFile
	for _, group := range [][]*graph.Node{{nodes[0], nodes[1]}, {nodes[2]}, {nodes[3]}, {nodes[4]}} {
		fileNode := group[len(group)-1]
		manifestFiles = append(manifestFiles, ManifestFile{Path: fileNode.FilePath, Language: fileNode.Language, Kind: 'f'})
		require.NoError(t, worker.AddParsedFile(fileNode.RepoPrefix, ParseFacts{
			File: graph.FileMetaRow{FilePath: fileNode.FilePath, NodeCount: len(group)}, Nodes: group,
		}))
	}
	manifest, err := PrepareManifest(ctx, t.TempDir(), manifestFiles, Witness{
		Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest,
		PlatformCatalogRevision: graph.PlatformCatalogRevision,
	}, testHeadIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	fileStage, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	files, err := worker.RetainFileGraph(ctx, fileStage)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, fileStage)
	require.NoError(t, err)
	scope.ReadSource = func(path string) ([]byte, error) { return config[path], nil }
	count, err := contracts.BindSpringConfig(native, scope)
	require.NoError(t, err)
	require.Equal(t, 6, count, "three nodes, two valid edges, one dangling native edge")
	present := scope.KeyID("app.httpPort")
	second := scope.KeyID("app.second")
	missing := scope.KeyID("env.only")
	deep := scope.KeyID("app.deep.other")
	require.Nil(t, native.GetNode(missing))
	require.Equal(t, firstPath, native.GetNode(present).FilePath)
	var valid []*graph.Edge
	var dangling *graph.Edge
	for _, e := range native.GetOutEdges(bean.ID) {
		if e.To == missing {
			dangling = e
		} else {
			valid = append(valid, e)
		}
	}
	require.NotNil(t, dangling)
	require.Len(t, valid, 2)

	relations, err := worker.PrepareRelations(ctx, t.TempDir(), files, nil)
	require.NoError(t, err)
	base, err := worker.RetainRelations(ctx, files, relations)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, relations)
	require.NoError(t, err)
	scope.ReadSource = func(string) ([]byte, error) {
		t.Fatal("late replay read the source")
		return nil, nil
	}
	var batches []RebuildContractBatch
	var paths []string
	var omitted []RebuildContractOmission
	// Use the same value-redacted capture as the checked parse receipts.
	captured := map[string][]*graph.Node{
		firstPath: contracts.SpringFileFacts(firstPath, config[firstPath]),
		laterPath: contracts.SpringFileFacts(laterPath, config[laterPath]),
	}
	stop := errors.New("config receipt read failed")
	require.NoError(t, worker.WithRebuildContractRecords(ctx, func(records *RebuildContractRecords) error {
		empty := scope
		empty.WorkspaceID = "other-workspace"
		if err := records.VisitSpringConfig(empty, func(string) ([]*graph.Node, error) { return nil, stop }, func(RebuildContractBatch) error {
			t.Fatal("emitted without a scoped file projection")
			return nil
		}); err != nil {
			return err
		}
		err := records.VisitSpringConfig(scope, func(string) ([]*graph.Node, error) { return nil, stop }, func(RebuildContractBatch) error {
			t.Fatal("published from a failed config receipt")
			return nil
		})
		require.ErrorIs(t, err, stop)
		return records.VisitSpringConfig(scope, func(path string) ([]*graph.Node, error) {
			paths = append(paths, path)
			return captured[path], nil
		}, func(batch RebuildContractBatch) error {
			require.Equal(t, 1, len(batch.Nodes)+len(batch.Edges)+len(batch.Omitted))
			omitted = append(omitted, batch.Omitted...)
			batches = append(batches, batch)
			return nil
		})
	}))
	require.Equal(t, []string{firstPath, laterPath}, paths)
	require.Equal(t, []RebuildContractOmission{{RepoPrefix: "r", FilePath: beanFile,
		Type: contracts.ContractType(graph.EdgeReadsConfig), ID: missing, RecordKey: HeadDigest(sha256.Sum256(pendingEdgeKey(dangling))),
	}}, omitted)
	stage, err := worker.PrepareContracts(ctx, t.TempDir(), base, contractBatches(batches...), ContractCoverage{OmittedRecords: 1})
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	snap := stageRead(t, head)
	for _, id := range []string{present, second, deep} {
		require.Equal(t, native.GetNode(id), snap.GetNode(id))
	}
	require.Nil(t, snap.GetNode(missing))
	require.ElementsMatch(t, valid, snap.GetOutEdges(bean.ID))
	read := contracts.ReadScope{RepoPrefixes: []string{"r"}, FilePaths: []string{beanFile}, Types: []contracts.ContractType{contracts.ContractType(graph.EdgeReadsConfig)}}
	partial, err := snap.ContractCoverageContext(ctx, read, false)
	require.NoError(t, err)
	require.True(t, partial)
	partial, err = snap.ContractCoverageContext(ctx, read, true)
	require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
	require.True(t, partial)
	read.ContractIDs = []string{present}
	partial, err = snap.ContractCoverageContext(ctx, read, true)
	require.NoError(t, err)
	require.False(t, partial, "present target scope is not partial")
}
