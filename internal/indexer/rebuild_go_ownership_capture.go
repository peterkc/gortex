package indexer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/zeebo/blake3"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/resolver"
)

var (
	errRebuildGoEvidence     = errors.New("missing exact frozen Go evidence")
	errRebuildGoCaptureLimit = errors.New("frozen Go evidence exceeds its capture limit")
)

const (
	rebuildGoManifestLimit = 64 << 10 // native ownership read limit
	rebuildGoPathLimit     = 4096
	rebuildGoKeyLimit      = 32768
	rebuildGoFactLimit     = 8 << 20 // retained paths and normalized string payload, not encoded bytes
)

// rebuildGoManifest is one portable positive or checked-negative probe. An
// omitted key is not a negative. RootClaim is the root-only loose module claim;
// OwnerModule is the strict nearest-module certificate, never a label or name.
// The root's direct dependencies are bridge trigger facts, not graph nodes.
// Replace lists only root rules that can affect the native extern module tier.
type rebuildGoManifest struct {
	Path         string
	Present      bool
	Digest       rebuildDigest
	RootClaim    string
	OwnerModule  string
	DirectDeps   []string
	ContractDeps []rebuildGoContractDep
	Replace      []rebuildGoReplace
}

type rebuildGoReplace struct {
	Module string
	Local  bool // canonical target and tracked-root equivalence not captured
}

// captureRebuildGoManifests is called before reuse against the same frozen
// checkout as inputs. observe may return (nil, nil) only for checked absence.
// A present identity must have cleared admission, including regular parent
// paths. Missing authority (including ignored paths and uncertain ancestry)
// is an error. read accepts only the observed identity and may not follow a
// changed or unadmitted path. Nothing here discovers paths on disk.
//
// The root's claim/replace/bridge inputs and the native ancestor probes for
// admitted Go directories are the only rows. No Go-file or package list is
// duplicated: the replay reducer uses the final checked parse/file rows.
// emit is tentative until this call and the owner's sidecar seal succeed.
func captureRebuildGoManifests(ctx context.Context, inputs []rebuildInput, rawRemaining *int64,
	observe func(context.Context, string) (*rebuildInput, error),
	read func(context.Context, rebuildInput) ([]byte, error), emit func(rebuildGoManifest) error,
) error {
	if rawRemaining == nil || *rawRemaining < 0 || observe == nil || read == nil || emit == nil {
		return fmt.Errorf("%w: incomplete manifest capture authority", errRebuildGoEvidence)
	}
	left := rebuildGoFactLimit
	dirs := make(map[string]struct{})
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if in.Language != "go" {
			continue
		}
		if !rebuildGoLocalPath(in.Path) || in.Kind != merkle.RegularFile {
			return fmt.Errorf("%w: unproved regular Go file %q", errRebuildGoEvidence, in.Path)
		}
		dir := path.Dir(in.Path)
		if rebuildGoVendorDir(dir) {
			continue
		}
		if _, seen := dirs[dir]; !seen {
			left -= len(dir)
			if len(dirs) == rebuildGoKeyLimit || left < 0 {
				return errRebuildGoCaptureLimit
			}
			dirs[dir] = struct{}{}
		}
	}
	rows := make(map[string]rebuildGoManifest)
	probe := func(name string) (rebuildGoManifest, error) {
		if row, ok := rows[name]; ok {
			return row, nil
		}
		if len(rows) == rebuildGoKeyLimit || len(name) > rebuildGoPathLimit || len(name) > left {
			return rebuildGoManifest{}, errRebuildGoCaptureLimit
		}
		row, err := captureRebuildGoManifest(ctx, name, rawRemaining, observe, read)
		if err != nil {
			return rebuildGoManifest{}, err
		}
		left -= rebuildGoManifestCharge(row)
		if left < 0 {
			return rebuildGoManifest{}, errRebuildGoCaptureLimit
		}
		rows[name] = row
		return row, nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := probe("go.mod"); err != nil {
		return err
	}
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := rebuildGoNearest(dir, probe); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(rows[name]); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func captureRebuildGoManifest(ctx context.Context, name string, rawRemaining *int64,
	observe func(context.Context, string) (*rebuildInput, error), read func(context.Context, rebuildInput) ([]byte, error),
) (rebuildGoManifest, error) {
	row := rebuildGoManifest{Path: name}
	if err := ctx.Err(); err != nil {
		return row, err
	}
	in, err := observe(ctx, name)
	if err != nil {
		return row, fmt.Errorf("%w: observe %q: %w", errRebuildGoEvidence, name, err)
	}
	if in == nil {
		return row, nil
	}
	if in.Path != name || in.Kind != merkle.RegularFile || in.Size < 0 || in.Content == (rebuildDigest{}) {
		return row, fmt.Errorf("%w: unproved regular manifest %q", errRebuildGoEvidence, name)
	}
	row.Present, row.Digest = true, in.Content
	if path.Base(name) == "go.work" {
		return row, nil // native asks only whether this path blocks certification
	}
	if in.Size > min(rebuildGoManifestLimit, *rawRemaining) {
		return row, fmt.Errorf("%w: read %q", errRebuildGoCaptureLimit, name)
	}
	data, err := read(ctx, *in)
	if err != nil {
		return row, fmt.Errorf("%w: read %q: %w", errRebuildGoEvidence, name, err)
	}
	if int64(len(data)) != in.Size || rebuildDigest(blake3.Sum256(data)) != in.Content {
		return row, fmt.Errorf("%w: %s", ErrBuildInputChanged, name)
	}
	*rawRemaining -= in.Size
	parsed, parseErr := modfile.Parse(name, data, nil)
	if parseErr == nil && parsed.Module != nil && len(parsed.Replace) == 0 {
		if p := parsed.Module.Mod.Path; rebuildGoModulePath(p) {
			row.OwnerModule = p
		}
	}
	if name != "go.mod" {
		return row, nil // a malformed or replaced nearest boundary is native Unknown
	}
	row.RootClaim = modfile.ModulePath(data)
	if row.RootClaim != "" && !rebuildGoModulePath(row.RootClaim) {
		return row, fmt.Errorf("%w: unsupported root module claim", errRebuildGoEvidence)
	}
	if parseErr != nil {
		return row, fmt.Errorf("%w: root replacement/dep authority: %w", errRebuildGoEvidence, parseErr)
	}
	for _, r := range parsed.Replace {
		if r.New.Version == "" || r.Old.Path != r.New.Path {
			if !rebuildGoModulePath(r.Old.Path) {
				return row, fmt.Errorf("%w: unsupported root replacement module", errRebuildGoEvidence)
			}
			row.Replace = append(row.Replace, rebuildGoReplace{Module: r.Old.Path, Local: r.New.Version == ""})
		}
	}
	slices.SortFunc(row.Replace, func(a, b rebuildGoReplace) int {
		if c := strings.Compare(a.Module, b.Module); c != 0 {
			return c
		}
		if a.Local == b.Local {
			return 0
		}
		if a.Local {
			return 1
		}
		return -1
	})
	row.Replace = slices.Compact(row.Replace)
	if err := row.captureContractDeps(data); err != nil {
		return rebuildGoManifest{}, err
	}
	return row, nil
}

func rebuildGoLocalPath(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= rebuildGoPathLimit &&
		path.Clean(name) == name && !strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/") &&
		!strings.ContainsAny(name, "\\\x00")
}

func rebuildGoModulePath(p string) bool {
	return rebuildGoLocalPath(p) && module.CheckImportPath(p) == nil
}

func rebuildGoVendorDir(dir string) bool {
	return slices.Contains(strings.Split(dir, "/"), "vendor")
}

// rebuildGoNearest shares the native workspace-before-module stop order.
// An empty returned OwnerModule is an exact native non-certificate when all
// visited rows were covered, including a known workspace or vendor boundary.
// For replay, lookup must return an error for an omitted/unverified key.
func rebuildGoNearest(dir string, lookup func(string) (rebuildGoManifest, error)) (rebuildGoManifest, error) {
	if rebuildGoVendorDir(dir) {
		return rebuildGoManifest{}, nil
	}
	checked := func(name string) (rebuildGoManifest, error) {
		row, err := lookup(name)
		if err == nil && row.Path != name {
			return rebuildGoManifest{}, fmt.Errorf("%w: manifest probe %q not covered", errRebuildGoEvidence, name)
		}
		return row, err
	}
	for ancestor := dir; ; ancestor = path.Dir(ancestor) {
		row, err := checked(path.Join(ancestor, "go.work"))
		if err != nil || row.Present {
			return rebuildGoManifest{}, err
		}
		if ancestor == "." {
			break
		}
	}
	for ancestor := dir; ; ancestor = path.Dir(ancestor) {
		row, err := checked(path.Join(ancestor, "go.mod"))
		if err != nil || row.Present || ancestor == "." {
			return row, err
		}
	}
}

// rebuildGoOwnership evaluates the native same-repo ownership gate against
// two enclosed graph identities. hasGoFile must use the checked KindFile/go
// inventory of this parse epoch, with exact graph paths and explicit negatives.
// A miss from a partial inventory is an error, not native Unknown. rawPath
// must map a graph-relative file to its exact admitted receipt path or refuse.
func rebuildGoOwnership(candidate resolver.GoImportCandidate, hasGoFile func(string) (bool, error),
	rawPath func(string) (string, error), manifest func(string) (rebuildGoManifest, error),
) (resolver.GoPackageOwnershipResult, error) {
	unknown := resolver.GoPackageOwnershipUnknown
	if candidate.CandidateRepoPrefix != candidate.ImporterRepoPrefix || module.CheckImportPath(candidate.ImportPath) != nil {
		return unknown, nil
	}
	if n := candidate.CandidateNode; n != nil && (n.Language != "go" || n.Kind == graph.KindContract) {
		return unknown, nil
	}
	if candidate.CandidateFilePath == "" { // native gate does not call its provider
		return unknown, nil
	}
	packageOf := func(file string) (string, error) {
		rel, owned := builderRelPath(candidate.ImporterRepoPrefix, file)
		if !owned || !rebuildGoLocalPath(rel) {
			return "", fmt.Errorf("%w: unproved Go file path %q", errRebuildGoEvidence, file)
		}
		ok, err := hasGoFile(file)
		if err != nil || !ok {
			return "", err
		}
		if rawPath == nil {
			return "", fmt.Errorf("%w: no raw receipt lookup for %q", errRebuildGoEvidence, rel)
		}
		raw, err := rawPath(rel)
		if err != nil {
			return "", fmt.Errorf("%w: raw receipt for %q: %w", errRebuildGoEvidence, rel, err)
		}
		if !rebuildGoLocalPath(raw) || pathkey.Normalize(raw) != rel {
			return "", fmt.Errorf("%w: raw receipt %q does not prove Go file %q", errRebuildGoEvidence, raw, rel)
		}
		owner, err := rebuildGoNearest(path.Dir(raw), manifest)
		if err != nil || owner.OwnerModule == "" {
			return "", err
		}
		// The raw owner is a proved ancestor. Drop its component count from
		// the graph directory to keep the native suffix spelling.
		suffix := path.Dir(rel)
		for base := path.Dir(owner.Path); base != "."; base = path.Dir(base) {
			_, suffix, _ = strings.Cut(suffix, "/")
		}
		return path.Join(owner.OwnerModule, suffix), nil
	}
	if importer, err := packageOf(candidate.ImporterFilePath); err != nil || importer == "" {
		return unknown, err
	}
	identity, err := packageOf(candidate.CandidateFilePath)
	if err != nil || identity == "" {
		return unknown, err
	}
	if identity == candidate.ImportPath {
		return resolver.GoPackageOwnershipExact, nil
	}
	return resolver.GoPackageOwnershipDifferent, nil
}

// rootReplaceBlocked answers the matching root replacement restriction, not
// candidate selection. A local replacement needs a separate captured canonical
// directory/registry fact, even when its target is missing or a tracked repo.
func (row rebuildGoManifest) rootReplaceBlocked(importPath string) (bool, error) {
	var match rebuildGoReplace
	for _, r := range row.Replace {
		if len(r.Module) > len(match.Module) && (importPath == r.Module || strings.HasPrefix(importPath, r.Module+"/")) {
			match = r
		}
	}
	if match.Local {
		return false, fmt.Errorf("%w: local Go replacement for %q needs a canonical directory and tracked-root membership", errRebuildGoEvidence, match.Module)
	}
	return match.Module != "", nil
}

// rootIndexedPackageExists is only the exact directory-membership arm of
// native tracked-package admission. It does not certify a module, choose a
// symbol, admit a vendor suffix or apply a workspace rule. Directory evidence
// must come from a complete same-epoch Go file group, not a failed name lookup.
func (row rebuildGoManifest) rootIndexedPackageExists(repoPrefix, importPath string, hasGoDir func(string) (bool, error)) (bool, error) {
	if row.Path != "go.mod" {
		return false, fmt.Errorf("%w: root claim not covered", errRebuildGoEvidence)
	}
	claim := row.RootClaim
	if claim == "" || (importPath != claim && !strings.HasPrefix(importPath, claim+"/")) {
		return false, nil
	}
	return hasGoDir(repoPrefix + strings.TrimPrefix(importPath, claim))
}

// rootBridgeClaim finds a potential positive only. A hit requires the native
// pre-resolve contract seed view, including tracked-repo aliases and existing
// node collisions, before a caller can choose a terminal or retarget an edge.
func (row rebuildGoManifest) rootBridgeClaim(importPath string) string {
	for p := importPath; ; p = p[:strings.LastIndexByte(p, '/')] {
		if _, ok := slices.BinarySearch(row.DirectDeps, p); ok {
			return p
		}
		if !strings.Contains(p, "/") {
			return ""
		}
	}
}
