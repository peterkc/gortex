// Package toolref renders references to Gortex tools for agent guidance.
// MCP-capable profiles get an explicit native-MCP requirement and must surface
// a missing callable handle as an integration failure. Bash-only profiles and
// CLI diagnostics can separately render the real `gortex call <tool> --arg
// k=v` shape, never the invalid bare `gortex <tool>` form.
package toolref

// cliExample maps internal/compatibility tool names used by hooks to the
// compact public call an agent should execute from Bash. Hooks may still use
// the internal handler name for their daemon probe; emitted guidance must not
// make an agent learn that implementation vocabulary.
//
// Symbol-ID placeholders render both forms — `<file>::<Name|Recv.Name>` — so
// an agent targeting a method knows the ID carries the receiver
// (`pkg/s.go::Server.Handle`), not the bare method name; the bare form only
// resolves functions and types.
var cliExample = map[string]string{
	"read_file":           `gortex call read --arg target='{"file":"<file>"}'`,
	"get_symbol_source":   `gortex call read --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"get_editing_context": `gortex call read --arg operation=editing_context --arg target='{"file":"<file>"}'`,
	"get_file_summary":    `gortex call read --arg operation=summary --arg target='{"file":"<file>"}'`,
	"get_symbol":          `gortex call read --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"search_symbols":      `gortex call search --arg operation=symbols --arg query='<name>'`,
	"search_text":         `gortex call search --arg operation=text --arg query='<text>'`,
	"find_usages":         `gortex call relations --arg operation=usages --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"get_callers":         `gortex call relations --arg operation=callers --arg target='{"symbol":"<file>::<Name|Recv.Name>"}'`,
	"smart_context":       `gortex call explore --arg operation=context --arg task='<task>'`,
	"explore":             `gortex call explore --arg task='<task>'`,
	"get_repo_outline":    `gortex call explore --arg operation=outline --arg options='{"path_prefix":"<dir>/"}'`,
	"edit_file":           `gortex call edit --arg target='{"file":"<file>"}' --arg match='<old>' --arg replacement='<new>'`,
	"edit_symbol":         `gortex call edit --arg target='{"symbol":"<id>"}' --arg match='<old>' --arg replacement='<new>'`,
	"index_repository":    `gortex call workspace_admin --arg operation=index --arg arguments='{"path":"<repo-root>"}'`,
	"reindex_repository":  `gortex call workspace_admin --arg operation=reindex --arg arguments='{"path":"<repo-root>"}'`,
}

// CLIFallback renders the compact shell invocation for one operation, for
// example `gortex call read --arg target='{"file":"..."}'`. This is the single
// place an internal tool reference becomes agent-facing Bash — nothing else
// should hand-assemble a `gortex …` shape, so the bare-verb mistake can never
// be re-minted piecemeal.
func CLIFallback(tool string) string {
	if example := cliExample[tool]; example != "" {
		return example
	}
	return "gortex call " + tool + " --arg key=value"
}

// ConcreteCLIFallback reports whether tool has a reviewed, shell-safe compact
// invocation rather than the generic compatibility fallback.
func ConcreteCLIFallback(tool string) (string, bool) {
	example, ok := cliExample[tool]
	return example, ok
}

// MCPMissingToolsException bounds the "integration failure, stop" rule to
// where it belongs: indexed code in a Gortex-tracked repo. In a directory no
// tracked repo covers (an agent-created clone, a scratch checkout) the graph
// tools are legitimately absent, and an explicit user instruction to proceed
// without Gortex must win; otherwise the guidance is a hard stop the user
// cannot lift (#900). The instruction profiles, the shared agent
// InstructionsBody, and MCPRequiredLine append this sentence verbatim.
const MCPMissingToolsException = "Outside a Gortex-tracked repo, or when the user says to proceed without Gortex, say so once and continue with native tools."

// MCPRequiredLine is the standard transport rule appended to guidance emitted
// by hooks installed for an MCP-capable profile. A configured server whose
// tools are absent from the model's callable manifest is a host integration
// failure, not permission to launch infrastructure or switch transports —
// except outside a tracked repo or on an explicit user override.
func MCPRequiredLine() string {
	return "  - Native Gortex MCP is mandatory for this profile. If a referenced Gortex tool is missing from the callable MCP tools despite the server being configured, surface a Gortex MCP integration failure to the user and stop. Do not start a daemon or use any CLI/shell fallback. " + MCPMissingToolsException + "\n"
}
