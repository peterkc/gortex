package toolref

import (
	"strings"
	"testing"
)

func TestCLIFallbackUsesCompactPublicSurface(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"read_file":         "gortex call read --arg target=",
		"get_symbol_source": "gortex call read --arg target=",
		"search_symbols":    "gortex call search --arg operation=symbols",
		"find_usages":       "gortex call relations --arg operation=usages",
		"smart_context":     "gortex call explore --arg operation=context",
		"edit_file":         "gortex call edit --arg target=",
		"index_repository":  "gortex call workspace_admin --arg operation=index",
	}
	for internal, want := range tests {
		internal, want := internal, want
		t.Run(internal, func(t *testing.T) {
			t.Parallel()
			got := CLIFallback(internal)
			if !strings.Contains(got, want) {
				t.Fatalf("CLIFallback(%q) = %q, want compact call containing %q", internal, got, want)
			}
			if strings.Contains(got, "gortex call "+internal+" ") {
				t.Fatalf("CLIFallback(%q) leaked the internal tool name: %q", internal, got)
			}
		})
	}
}

func TestMCPRequiredLineRejectsTransportFallback(t *testing.T) {
	t.Parallel()

	got := MCPRequiredLine()
	for _, want := range []string{
		"Native Gortex MCP is mandatory",
		"surface a Gortex MCP integration failure",
		"Do not start a daemon",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("MCPRequiredLine() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "gortex call ") {
		t.Fatalf("MCP-required guidance advertised a CLI fallback: %q", got)
	}
}

// TestMCPRequiredLineIsNotAnUnliftableStop pins #900: the stop rule must carry
// its exception, so an agent in an untracked directory (or told by the user to
// proceed without Gortex) continues with native tools instead of refusing.
func TestMCPRequiredLineIsNotAnUnliftableStop(t *testing.T) {
	t.Parallel()

	got := MCPRequiredLine()
	for _, want := range []string{
		MCPMissingToolsException,
		"Outside a Gortex-tracked repo",
		"proceed without Gortex",
		"continue with native tools",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("MCPRequiredLine() = %q, want %q", got, want)
		}
	}
}

func TestCLIFallbackPlaceholdersAreShellQuoted(t *testing.T) {
	t.Parallel()
	for name, example := range cliExample {
		if strings.Contains(example, "=<") {
			t.Errorf("cliExample[%q] contains an unquoted shell-redirection placeholder: %s", name, example)
		}
	}
}
