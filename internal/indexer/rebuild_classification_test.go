package indexer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildClassificationFileGraph(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	lease := t.TempDir()
	cfg := config.Default().Index
	cfg.MaxFileSize = 128
	const big, named, unrecognized, empty, parsed = "large.custom", "large.go", "plain.custom", "empty.custom", "small.custom"
	const shebang = "#!/usr/bin/env python3\n"
	bigData := shebang + strings.Repeat("# padding\n", 60)
	smallData := shebang + "def frozen_declaration():\n    return 1\n"
	for name, data := range map[string]string{big: bigData, named: bigData, unrecognized: "plain text", empty: "", parsed: smallData} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(data), 0o600))
	}
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	_, known := reg.DetectLanguage(big)
	require.False(t, known)
	prefix := []byte(bigData[:512])
	lang, recognized := reg.DetectLanguageContent(big, prefix)
	require.True(t, recognized)
	require.Equal(t, "python", lang)
	leaf, _, err := readRebuildInput(ctx, src, parsed, lang, cfg.MaxFileSize)
	require.NoError(t, err)
	rows := []rebuildSidecarRow{
		rebuildClassificationSidecarRow(big, lang, int64(len(bigData)), cfg.MaxFileSize, prefix, true),
		rebuildClassificationSidecarRow(named, "go", int64(len(bigData)), cfg.MaxFileSize, nil, false),
		rebuildClassificationSidecarRow(unrecognized, "", 10, cfg.MaxFileSize, []byte("plain text"), true),
		rebuildClassificationSidecarRow(empty, "", 0, cfg.MaxFileSize, nil, true),
		rebuildClassificationSidecarRow(parsed, lang, leaf.Size, cfg.MaxFileSize, []byte(smallData), true),
	}
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
		for _, r := range rows {
			if err := emit(r); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	req := writeRebuildClassificationTestRequest(t, lease, root, cfg, digest, []rebuildInput{leaf})
	// The recognized file no longer has a language, and other candidates
	// are gone. Only the admitted parse leaf remains available to read.
	require.NoError(t, os.WriteFile(filepath.Join(root, big), []byte("changed"), 0o600))
	for _, name := range []string{named, unrecognized, empty} {
		require.NoError(t, os.Remove(filepath.Join(root, name)))
	}
	h, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), req.Incarnation, req.Witness.Profile)
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	var sent []store_pebble.Stage
	err = buildRebuildStages(ctx, lease, zap.NewNop(), store_pebble.OpenWorkerBuild, func(tables store_pebble.StageTables) error {
		sent = append(sent, tables.Witness.Stage)
		require.Equal(t, req.Witness.FrozenInputs, tables.Witness.FrozenInputs)
		_, err := h.PublishStage(ctx, tables)
		return err
	})
	require.ErrorIs(t, err, errRebuildBuildIncomplete)
	require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, sent)
	s := h.OpenSnapshot()
	defer func() { require.NoError(t, s.Close()) }()
	require.NoError(t, s.RequireStage(store_pebble.StageFileGraph))
	for name, lang := range map[string]string{big: "python", named: "go"} {
		want := &graph.Node{
			ID: name, Kind: graph.KindFile, Name: name, FilePath: name, Language: lang,
			Meta: map[string]any{
				"skip_reason": "size", "skipped_due_to_size": true,
				"file_size_bytes": int64(len(bigData)), "max_file_size_bytes": int64(128),
			},
		}
		require.Equal(t, want, s.GetNode(name))
	}
	for _, name := range []string{big, named, unrecognized, empty} {
		meta, err := s.FileMetadata("", name)
		require.NoError(t, err)
		require.Empty(t, meta, "classification is not a parse receipt for %q", name)
	}
	require.Nil(t, s.GetNode(unrecognized))
	require.Nil(t, s.GetNode(empty))
	require.Len(t, s.FindNodesByName("frozen_declaration"), 1)
	parsedNode := s.GetNode(parsed)
	require.NotNil(t, parsedNode)
	require.NotContains(t, parsedNode.Meta, "skipped_due_to_size")
	var manifest []string
	require.NoError(t, s.ManifestFiles(ctx, func(f store_pebble.ManifestFile) error {
		manifest = append(manifest, f.Path)
		return nil
	}))
	require.Equal(t, []string{parsed}, manifest)
	saved, err := os.ReadDir(req.ParseDir)
	require.NoError(t, err)
	require.Len(t, saved, 1)
}

func TestRebuildClassificationSidecarPathAndEmptyProof(t *testing.T) {
	ctx := t.Context()
	lease := t.TempDir()
	const key = "dir/no-language-\xff"
	row := rebuildClassificationSidecarRow(key, "", 0, 2, nil, true)
	digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error { return emit(row) })
	require.NoError(t, err)
	visited := 0
	require.NoError(t, visitRebuildInputSidecar(ctx, lease, digest, func(got rebuildSidecarRow) error {
		visited++
		require.Equal(t, rebuildClassificationRow, got.Kind)
		require.Equal(t, key, string(got.Path))
		require.Equal(t, &rebuildClassificationFact{MaxFileSize: 2, PrefixDigest: blake3.Sum256(nil)}, got.Classification)
		return nil
	}))
	require.Equal(t, 1, visited)
}

func TestRebuildClassificationRefusesBeforeManifest(t *testing.T) {
	cfg := config.Default().Index
	cfg.MaxFileSize = 128
	bigPrefix := []byte("#!/bin/sh\n" + strings.Repeat("#", 502))
	for _, tc := range []struct {
		name    string
		change  func(*rebuildSidecarRow)
		variant string
	}{
		{name: "path alias", change: func(row *rebuildSidecarRow) { row.Path = []byte("dir/../escape") }},
		{name: "prefix exceeds cap", change: func(row *rebuildSidecarRow) { row.Classification.PrefixBytes = 513 }},
		{name: "prefix exceeds size", change: func(row *rebuildSidecarRow) { row.Classification.Size = 511 }},
		{name: "negative size", change: func(row *rebuildSidecarRow) { row.Classification.Size = -1 }},
		{name: "missing prefix digest", change: func(row *rebuildSidecarRow) { row.Classification.PrefixDigest = rebuildDigest{} }},
		{name: "untrimmed language", change: func(row *rebuildSidecarRow) { row.Classification.Language = " bash" }},
		{name: "unknown language", change: func(row *rebuildSidecarRow) { row.Classification.Language = "not-registered" }},
		{name: "mismatched configured limit", change: func(row *rebuildSidecarRow) { row.Classification.MaxFileSize++ }},
		{name: "duplicate row", variant: "duplicate"},
		{name: "digest mismatch", variant: "digest"},
		{name: "row above encoded limit", variant: "wide"},
		{name: "conflicting parse leaf", variant: "overlap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, lease := t.TempDir(), t.TempDir()
			const key = "runner.custom"
			row := rebuildClassificationSidecarRow(key, "bash", 1024, cfg.MaxFileSize, bigPrefix, true)
			if tc.change != nil {
				tc.change(&row)
			}
			// Seed unchecked wire bytes so replay must catch structural defects
			// even when the outer digest correctly names the malformed sidecar.
			line, err := json.Marshal(row)
			require.NoError(t, err)
			data := rebuildInputSidecarMagic + string(line) + "\n"
			var inputs []rebuildInput
			switch tc.variant {
			case "duplicate":
				data += string(line) + "\n"
			case "wide":
				data += strings.Repeat(" ", rebuildCppEntryLimit) + "\n"
			case "overlap":
				inputs = append(inputs, rebuildInput{Path: key, Language: "bash", Kind: merkle.RegularFile, Size: 1024, Content: rebuildDigest{22}})
			}
			require.NoError(t, os.WriteFile(filepath.Join(lease, rebuildInputSidecarName), []byte(data), 0o600))
			digest := blake3.Sum256([]byte(data))
			if tc.variant == "digest" {
				digest[0] ^= 1
			}
			req := writeRebuildClassificationTestRequest(t, lease, root, cfg, digest, inputs)
			sent := 0
			err = buildRebuildStages(ctx, lease, zap.NewNop(), store_pebble.OpenWorkerBuild, func(store_pebble.StageTables) error {
				sent++
				return nil
			})
			require.ErrorIs(t, err, errRebuildInputSidecar)
			refusal := buildRefusal(err)
			require.Zero(t, sent)
			_, err = os.Stat(filepath.Join(lease, store_pebble.StageManifest.String()))
			require.ErrorIs(t, err, os.ErrNotExist)
			saved, err := os.ReadDir(req.ParseDir)
			require.NoError(t, err)
			require.Empty(t, saved)
			// The existing child/parent wire exposes the same typed refusal.
			var reply strings.Builder
			require.NoError(t, writeRebuildBuildReply(&reply, refusal))
			var ack strings.Builder
			err = receiveRebuildStages(ctx, lease, req, strings.NewReader(reply.String()), &ack, func(context.Context, store_pebble.StageTables) error {
				t.Fatal("classification refusal offered a stage")
				return nil
			})
			require.ErrorIs(t, err, errRebuildInputSidecar)
			require.Zero(t, ack.Len())
		})
	}
}

func writeRebuildClassificationTestRequest(t *testing.T, lease, root string, cfg config.IndexConfig, frozen rebuildDigest, inputs []rebuildInput) rebuildBuildRequest {
	t.Helper()
	rootHex, err := rebuildInputRoot(inputs)
	require.NoError(t, err)
	raw, err := hex.DecodeString(rootHex)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: 2,
		BaseRecipe: store_pebble.HeadDigest{2}, Recipes: map[string]rebuildDigest{"python": {1}, "bash": {1}},
		Policy: freezeRebuildParsePolicy(root, cfg), Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 64 << 10},
		Witness: store_pebble.Witness{
			Input: store_pebble.HeadDigest(raw), FrozenInputs: store_pebble.HeadDigest(frozen),
			Profile: store_pebble.DefaultProfile, Stage: store_pebble.StageManifest,
		},
	}
	req.Witness.Recipe = rebuildProjectRecipe(req.BaseRecipe, req.ProjectName, req.Witness.FrozenInputs)
	require.NoError(t, writeRebuildBuildRequest(t.Context(), lease, req, cfg, inputs))
	return req
}
