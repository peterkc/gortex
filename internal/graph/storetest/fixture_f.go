package storetest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Fixture F of design-c.md 1.8: repositories R and S in git, two checkouts
// of R, same-named packages and types (api.Client in both), a
// dependency-selected call R:Run -> S:Serve, one failed name lookup
// (Missing), a file with two edge contributions (app/twice.go), an
// empty-output producer (contracts) and an authored note and memory.
//
// Each repository has two committed versions. E1 changes the call target
// (S's Serve takes an argument) and a source file of R (app/run.go). The
// second checkout of R is a linked worktree that stays at E0.
//
// The graph facts below are the expected answers: they are what an extractor
// would produce from these sources, written out once, and every case compares
// what a backend serves with them.

// Epoch selects one of F's two committed versions.
type Epoch int

const (
	E0 Epoch = iota
	E1
)

// Checkout IDs of F.
const (
	CheckoutR1 = "checkout-r1" // R's primary working tree
	CheckoutR2 = "checkout-r2" // R's linked worktree, kept at E0
	CheckoutS1 = "checkout-s1" // S's working tree
)

// Repository is one git repository of F.
type Repository struct {
	// Prefix is the repository identity and graph repo prefix.
	Prefix string
	// Root is the primary working tree.
	Root string
	// Commit and Tree are the object IDs of E0 and E1.
	Commit [2]string
	Tree   [2]string
}

// AuthoredRecord is a note or memory a user wrote. K13 carries it across
// rebuild, rollback and cutover.
type AuthoredRecord struct {
	ID     string
	Kind   string
	Anchor string
	Text   string
}

// FixtureF is one instance of fixture F in its own temporary directory.
type FixtureF struct {
	R, S Repository
	// Checkouts lists R1, R2 and S1.
	Checkouts []Checkout
	Note      AuthoredRecord
	Memory    AuthoredRecord
}

// fixtureFSources are the files of each repository at E0 and E1.
var fixtureFSources = map[string][2]map[string]string{
	"s": {
		{
			"go.mod":        "module example.test/s\n\ngo 1.22\n",
			"api/client.go": "package api\n\n// Client is S's client.\ntype Client struct{}\n\n// Serve answers one request.\nfunc Serve() {}\n",
		},
		{
			"go.mod":        "module example.test/s\n\ngo 1.22\n",
			"api/client.go": "package api\n\n// Client is S's client.\ntype Client struct{}\n\n// Serve answers one numbered request.\nfunc Serve(n int) {}\n",
		},
	},
	"r": {
		{
			"go.mod":        "module example.test/r\n\ngo 1.22\n\nrequire example.test/s v0.0.0\n\nreplace example.test/s => ../s\n",
			"api/client.go": "package api\n\n// Client is R's client.\ntype Client struct{}\n",
			"app/run.go":    "package app\n\nimport sapi \"example.test/s/api\"\n\n// Run serves through S and calls a name nothing declares.\nfunc Run() {\n\tsapi.Serve()\n\tMissing()\n}\n",
			"app/twice.go":  "package app\n\nimport \"example.test/r/api\"\n\n// Twice calls Run and returns R's Client.\nfunc Twice() api.Client {\n\tRun()\n\treturn api.Client{}\n}\n",
		},
		{
			"go.mod":        "module example.test/r\n\ngo 1.22\n\nrequire example.test/s v0.0.0\n\nreplace example.test/s => ../s\n",
			"api/client.go": "package api\n\n// Client is R's client.\ntype Client struct{}\n",
			"app/run.go":    "package app\n\nimport sapi \"example.test/s/api\"\n\n// Run serves through S and calls a name nothing declares.\nfunc Run() {\n\t// S now numbers each request.\n\tsapi.Serve(1)\n\tMissing()\n}\n",
			"app/twice.go":  "package app\n\nimport \"example.test/r/api\"\n\n// Twice calls Run and returns R's Client.\nfunc Twice() api.Client {\n\tRun()\n\treturn api.Client{}\n}\n",
		},
	},
}

// NewFixtureF writes R and S as git repositories under t.TempDir and commits
// both versions. It skips only when git is not installed (review rule T5).
func NewFixtureF(t *testing.T) *FixtureF {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("fixture F needs git: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "fixture f")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture-f@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "fixture f")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture-f@example.invalid")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve fixture directory: %v", err)
	}
	f := &FixtureF{
		R: Repository{Prefix: "r", Root: filepath.Join(base, "r")},
		S: Repository{Prefix: "s", Root: filepath.Join(base, "s")},
		Note: AuthoredRecord{
			ID: "note-run", Kind: "note", Anchor: "r/app/run.go::Run",
			Text: "Run must keep calling S's Serve.",
		},
		Memory: AuthoredRecord{
			ID: "memory-client", Kind: "memory", Anchor: "r/api/client.go::Client",
			Text: "R and S both declare api.Client.",
		},
	}
	worktree := filepath.Join(base, "r-feature")
	for _, repo := range []*Repository{&f.S, &f.R} {
		fixtureGit(t, base, "init", "--initial-branch=main", repo.Root)
		for epoch, files := range fixtureFSources[repo.Prefix] {
			writeFixtureFiles(t, repo.Root, files)
			fixtureGit(t, repo.Root, "add", "-A")
			fixtureGit(t, repo.Root, "commit", "-m", fmt.Sprintf("E%d", epoch))
			repo.Commit[epoch] = fixtureGit(t, repo.Root, "rev-parse", "HEAD")
			repo.Tree[epoch] = fixtureGit(t, repo.Root, "rev-parse", "HEAD^{tree}")
			if repo.Prefix == "r" && Epoch(epoch) == E0 {
				fixtureGit(t, repo.Root, "worktree", "add", "-b", "feature", worktree)
			}
		}
	}
	f.Checkouts = []Checkout{
		{Repository: "r", ID: CheckoutR1, Root: f.R.Root},
		{Repository: "r", ID: CheckoutR2, Root: worktree},
		{Repository: "s", ID: CheckoutS1, Root: f.S.Root},
	}
	return f
}

// RepositoryOf returns the repository a checkout belongs to.
func (f *FixtureF) RepositoryOf(checkout string) *Repository {
	if checkout == CheckoutS1 {
		return &f.S
	}
	return &f.R
}

// Inputs is the complete input identity of a checkout's version at epoch.
// R's dependency selection names S's commit at the same epoch.
func (f *FixtureF) Inputs(checkout string, epoch Epoch) Inputs {
	repo := f.RepositoryOf(checkout)
	inputs := Inputs{
		Tree:       repo.Tree[epoch],
		Commit:     repo.Commit[epoch],
		Config:     "config-f",
		Extractors: `{"go":"1"}`,
		Resolver:   "resolver-f",
	}
	if repo.Prefix == "r" {
		inputs.Dependencies = "example.test/s@" + f.S.Commit[epoch]
	}
	return inputs
}

// Producers is every producer of a version of F, sorted. contracts finishes
// with no output.
func (f *FixtureF) Producers() []string { return []string{"contracts", "extractor", "resolver"} }

// Payload is the complete graph contribution of repository prefix at epoch.
// Each call returns new values the caller may keep.
func (f *FixtureF) Payload(prefix string, epoch Epoch) Payload {
	nodes, edges, vectors := fixtureFGraph(prefix, epoch)
	p := Payload{Nodes: nodes, Edges: edges, Vectors: vectors, Producers: f.Producers()}
	for _, n := range nodes {
		p.Symbols = append(p.Symbols, graph.SymbolFTSItem{NodeID: n.ID, Tokens: strings.ToLower(n.Name)})
	}
	for _, rel := range slices.Sorted(maps.Keys(fixtureFSources[prefix][epoch])) {
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		body := fixtureFSources[prefix][epoch][rel]
		sum := sha256.Sum256([]byte(body))
		path := prefix + "/" + rel
		count := 0
		for _, n := range nodes {
			if n.FilePath == path {
				count++
			}
		}
		p.Files = append(p.Files, graph.FileMetaRow{
			FilePath: path, ContentHash: hex.EncodeToString(sum[:]), Size: len(body), NodeCount: count,
		})
	}
	return p
}

// fixtureFGraph is the nodes, edges and embeddings of one repository version.
func fixtureFGraph(prefix string, epoch Epoch) ([]*graph.Node, []*graph.Edge, []graph.VectorItem) {
	if prefix == "s" {
		signature, serveVec := "func Serve()", []float32{0, 1, 0}
		if epoch == E1 {
			signature, serveVec = "func Serve(n int)", []float32{0, 0.6, 0.8}
		}
		return []*graph.Node{
			{ID: "s/api/client.go::Client", Kind: graph.KindType, Name: "Client", QualName: "api.Client",
				FilePath: "s/api/client.go", StartLine: 4, EndLine: 4, Language: "go", RepoPrefix: "s"},
			{ID: "s/api/client.go::Serve", Kind: graph.KindFunction, Name: "Serve", QualName: "api.Serve",
				FilePath: "s/api/client.go", StartLine: 7, EndLine: 7, Language: "go", RepoPrefix: "s",
				Meta: map[string]any{"signature": signature}},
		}, nil, []graph.VectorItem{
			{NodeID: "s/api/client.go::Client", Vec: []float32{1, 0, 0}},
			{NodeID: "s/api/client.go::Serve", Vec: serveVec},
		}
	}
	// R: E1 adds a comment line inside Run, so its calls move down one line.
	runEnd, serveLine, missingLine, runVec := 9, 7, 8, []float32{0, 1, 0}
	if epoch == E1 {
		runEnd, serveLine, missingLine, runVec = 10, 8, 9, []float32{0, 0.8, 0.6}
	}
	return []*graph.Node{
		{ID: "r/api/client.go::Client", Kind: graph.KindType, Name: "Client", QualName: "api.Client",
			FilePath: "r/api/client.go", StartLine: 4, EndLine: 4, Language: "go", RepoPrefix: "r"},
		{ID: "r/app/run.go::Run", Kind: graph.KindFunction, Name: "Run", QualName: "app.Run",
			FilePath: "r/app/run.go", StartLine: 6, EndLine: runEnd, Language: "go", RepoPrefix: "r"},
		{ID: "r/app/twice.go::Twice", Kind: graph.KindFunction, Name: "Twice", QualName: "app.Twice",
			FilePath: "r/app/twice.go", StartLine: 6, EndLine: 9, Language: "go", RepoPrefix: "r"},
	}, []*graph.Edge{
		// The dependency-selected call into S.
		{From: "r/app/run.go::Run", To: "s/api/client.go::Serve", Kind: graph.EdgeCalls, FilePath: "r/app/run.go",
			Line: serveLine, Origin: graph.OriginLSPResolved, Tier: "lsp", CrossRepo: true},
		// The failed name lookup.
		{From: "r/app/run.go::Run", To: graph.UnresolvedMarker + "Missing", Kind: graph.EdgeCalls, FilePath: "r/app/run.go",
			Line: missingLine, Origin: graph.OriginASTInferred, Tier: "heuristic"},
		// Two contributions to one file: the extractor's call and the
		// resolver's reference.
		{From: "r/app/twice.go::Twice", To: "r/app/run.go::Run", Kind: graph.EdgeCalls, FilePath: "r/app/twice.go",
			Line: 7, Origin: graph.OriginASTResolved, Tier: "ast"},
		{From: "r/app/twice.go::Twice", To: "r/api/client.go::Client", Kind: graph.EdgeReferences, FilePath: "r/app/twice.go",
			Line: 8, Origin: graph.OriginLSPResolved, Tier: "lsp"},
	}, []graph.VectorItem{
		{NodeID: "r/api/client.go::Client", Vec: []float32{1, 0, 0}},
		{NodeID: "r/app/run.go::Run", Vec: runVec},
		{NodeID: "r/app/twice.go::Twice", Vec: []float32{0, 0, 1}},
	}
}

// fixtureGit runs git in dir and returns its trimmed output.
func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFixtureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}
