package serverstack

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/lspuri"
	"go.uber.org/zap"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/semantic/lsp"
)

// TestPreferredSpecName pins the resolve-time helper's server choice:
// the highest-priority spec the router reports available wins, with
// the node-based TypeScript server serving only when tsgo is absent.
func TestPreferredSpecName(t *testing.T) {
	availableSet := func(names ...string) func(*lsp.ServerSpec) bool {
		have := make(map[string]bool, len(names))
		for _, n := range names {
			have[n] = true
		}
		return func(s *lsp.ServerSpec) bool { return s != nil && have[s.Name] }
	}

	if got := preferredSpecName(availableSet("tsgo", "typescript-language-server"), ".ts"); got != "tsgo" {
		t.Errorf("both available: got %q, want tsgo", got)
	}
	if got := preferredSpecName(availableSet("typescript-language-server"), ".ts"); got != "typescript-language-server" {
		t.Errorf("tsgo absent: got %q, want typescript-language-server", got)
	}
	if got := preferredSpecName(availableSet(), ".ts"); got != "tsgo" {
		t.Errorf("none available: got %q, want priority winner tsgo", got)
	}
	if got := preferredSpecName(availableSet("pyright", "pyrefly"), ".py"); got != "pyright" {
		t.Errorf("python default: got %q, want pyright", got)
	}
	if got := preferredSpecName(availableSet(), ".unknown_ext"); got != "" {
		t.Errorf("unknown extension: got %q, want empty", got)
	}
}

func TestRepoLikelyHasPythonIntent(t *testing.T) {
	dir := t.TempDir()
	if RepoLikelyHasPythonIntent(dir) {
		t.Fatalf("empty temp dir should not look like a Python repo")
	}

	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RepoLikelyHasPythonIntent(dir) {
		t.Fatalf("pyproject.toml should mark a Python repo")
	}
}

func TestRepoLikelyHasPythonIntent_RootScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "script.py"), []byte("print('hello')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RepoLikelyHasPythonIntent(dir) {
		t.Fatalf("root-level .py file should mark a Python repo")
	}
}

func TestRepoLikelyHasCSharpIntent(t *testing.T) {
	dir := t.TempDir()
	if RepoLikelyHasCSharpIntent(dir) {
		t.Fatalf("empty temp dir should not look like a C# repo")
	}
	if err := os.WriteFile(filepath.Join(dir, "App.slnx"), []byte("<Solution/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RepoLikelyHasCSharpIntent(dir) {
		t.Fatalf("root-level .slnx should mark a C# repo")
	}

	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "App.csproj"), []byte("<Project/>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !RepoLikelyHasCSharpIntent(proj) {
		t.Fatalf("root-level .csproj should mark a C# repo")
	}

	nested := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nested, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "src", "App.sln"), []byte("sln\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if RepoLikelyHasCSharpIntent(nested) {
		t.Fatalf("intent probes are root-level only, like the TS/Python ones")
	}
}

func TestCSharpResolverHelperOptIn(t *testing.T) {
	t.Setenv(CSharpResolverEnv, "")
	if csharpResolverHelperEnabled() {
		t.Fatalf("resolve-time C# LSP must be opt-in: unset means disabled")
	}
	// Any set, non-falsy value enables — the same value vocabulary
	// IsFalsyEnv uses, so "on"/"yes" behave like the sibling
	// GORTEX_LSP_RESOLVER instead of being silently ignored.
	for _, v := range []string{"1", "true", "TRUE", "on", "yes"} {
		t.Setenv(CSharpResolverEnv, v)
		if !csharpResolverHelperEnabled() {
			t.Fatalf("%q should enable the resolve-time C# helper", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "n"} {
		t.Setenv(CSharpResolverEnv, v)
		if csharpResolverHelperEnabled() {
			t.Fatalf("%q should disable the resolve-time C# helper", v)
		}
	}
}

func TestBuildResolverLSPHelperHoldsRouterPinDuringCall(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			// Given the production default-mode helper and a real protocol peer.
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.ts"), []byte("target();\n"), 0o600))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			var conn net.Conn
			accepted := make(chan struct{})
			go func() {
				defer close(done)
				c, err := ln.Accept()
				conn = c
				close(accepted)
				if err != nil {
					return
				}
				defer c.Close()
				reader := bufio.NewReader(c)
				for {
					size := 0
					for {
						line, err := reader.ReadString('\n')
						if err != nil {
							return
						}
						if line == "\r\n" {
							break
						}
						if strings.HasPrefix(line, "Content-Length:") {
							size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
						}
					}
					body := make([]byte, size)
					if _, err := io.ReadFull(reader, body); err != nil {
						return
					}
					var req struct {
						ID     json.RawMessage
						Method string
					}
					if json.Unmarshal(body, &req) != nil {
						return
					}
					if len(req.ID) == 0 {
						continue
					}
					var result any
					switch req.Method {
					case "initialize":
						result = map[string]any{"capabilities": map[string]any{"definitionProvider": true}}
					case "textDocument/definition":
						close(entered)
						<-release
						result = []lsp.Location{{URI: lspuri.PathToURI(filepath.Join(root, "main.ts")), Range: lsp.Range{Start: lsp.Position{Line: 0}}}}
					}
					reply := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
					if fail && req.Method == "textDocument/definition" {
						delete(reply, "result")
						reply["error"] = map[string]any{"code": -32603, "message": "injected definition error"}
					}
					response, _ := json.Marshal(reply)
					if _, err := fmt.Fprintf(c, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
						return
					}
				}
			}()
			router := lsp.NewRouter(root, zap.NewNop())
			t.Cleanup(func() {
				unblock()
				_ = router.Close()
				_ = ln.Close()
				<-accepted
				if conn != nil {
					_ = conn.Close()
				}
				<-done
			})
			spec := &lsp.ServerSpec{Name: "definition-pin", Languages: []string{"typescript"}, Extensions: []string{".ts"}, Connect: &lsp.ConnectSpec{Network: "tcp", Address: ln.Addr().String()}}
			router.RegisterSpec(spec)
			helper := BuildResolverLSPHelper(router, spec, root, 1, zap.NewNop())
			type answer struct {
				path string
				line int
				ok   bool
			}
			answerCh := make(chan answer, 1)
			go func() { p, n, ok := helper.Definition("main.ts", 1, "target"); answerCh <- answer{p, n, ok} }()
			select {
			case <-entered:
			case <-t.Context().Done():
				t.Fatal("definition never entered")
			}
			// When cleanup runs during Definition, its pin keeps the provider open.
			stats := router.Stats()
			require.Len(t, stats, 1)
			assert.Equal(t, 1, stats[0].InUse)
			assert.Zero(t, router.CloseCheckoutWorkspace("typescript", root))
			unblock()
			got := <-answerCh
			assert.Equal(t, !fail, got.ok)
			if !fail {
				assert.Equal(t, "main.ts", got.path)
				assert.Equal(t, 1, got.line)
			}
			stats = router.Stats()
			if assert.Len(t, stats, 1) {
				assert.Zero(t, stats[0].InUse)
			}
			assert.Equal(t, 1, router.CloseCheckoutWorkspace("typescript", root))
		})
	}
}
