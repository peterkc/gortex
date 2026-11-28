package resolver

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// SynthPascalForm pairs a Delphi / Lazarus unit (.pas / .pp / .dpr / .lpr) with
// its same-directory, same-basename form definition (.dfm / .lfm / .fmx) — the
// two halves of a visual component the toolchain links by convention but that
// no symbol-level resolution can see.
const SynthPascalForm = "pascal-form"

// pascalFormVia marks an emitted unit→form pairing edge.
const pascalFormVia = "pascal_form"

// ResolvePascalForms emits a reference edge from each Pascal unit file node to
// its paired form file node. The pairing is workspace-scoped (a basename shared
// across unrelated repos never pairs) and rides a provenance tier — a strict
// improvement over a flat, repo-blind link. Idempotent: graph.AddEdge dedupes
// and graph.EvictFile drops the edge on reindex. Returns the number paired.
func ResolvePascalForms(g graph.Store) int {
	if g == nil {
		return 0
	}
	units := map[string]graph.FileNodeIdentity{}
	forms := map[string]graph.FileNodeIdentity{}
	for n := range graph.FileNodeIdentitiesSeq(g, nil) {
		switch pascalFormRole(n.FilePath) {
		case pascalUnitFile:
			units[pascalFormKey(n.FilePath)] = n
		case pascalDefinitionFile:
			forms[pascalFormKey(n.FilePath)] = n
		}
	}
	if len(units) == 0 || len(forms) == 0 {
		return 0
	}
	keys := make([]string, 0, len(units))
	for k := range units {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var batch []*graph.Edge
	for _, key := range keys {
		unit := units[key]
		form, ok := forms[key]
		if !ok {
			continue
		}
		e := pascalFormPair(unit, form)
		if e == nil {
			continue
		}
		e.Confidence = 0.85
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeReferences, 0.85)
		e.Origin = graph.OriginASTInferred
		e.Meta = map[string]any{
			"via":             pascalFormVia,
			MetaSynthesizedBy: SynthPascalForm,
			MetaProvenance:    ProvenanceHeuristic,
		}
		batch = append(batch, e)
	}
	for _, e := range batch {
		g.AddEdge(e)
	}
	return len(batch)
}

const (
	pascalUnitFile = iota + 1
	pascalDefinitionFile
)

func pascalFormRole(path string) int {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pas", ".pp", ".dpr", ".lpr":
		return pascalUnitFile
	case ".dfm", ".lfm", ".fmx":
		return pascalDefinitionFile
	}
	return 0
}

func pascalFormPair(unit, form graph.FileNodeIdentity) *graph.Edge {
	if unit.WorkspaceID != form.WorkspaceID {
		return nil
	}
	return &graph.Edge{From: unit.ID, To: form.ID, Kind: graph.EdgeReferences, FilePath: unit.FilePath}
}

// pascalFormKey is the directory plus lowercased extension-less basename that a
// paired unit and form share.
func pascalFormKey(path string) string {
	dir := filePathDir(path)
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return dir + "/" + strings.ToLower(base)
}
