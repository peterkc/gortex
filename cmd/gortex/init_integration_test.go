package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/zzet/gortex/internal/agents"
)

func TestInitSummaryDoesNotRequireManualMCPEnablement(t *testing.T) {
	var out bytes.Buffer
	emitHumanSummary(&out, nil, agents.ApplyOpts{})

	got := out.String()
	if !strings.Contains(got, "no separate MCP enablement is normally required") {
		t.Fatalf("init summary must describe automatic MCP registration:\n%s", got)
	}
	if strings.Contains(got, "enable the gortex server") {
		t.Fatalf("init summary still asks for manual MCP enablement:\n%s", got)
	}
}

// TestInitDryRunJSONReportShape is the end-to-end contract test for
// the non-interactive init flags. It runs runInit with --yes
// --dry-run --json --agents=claude-code against a clean temp dir
// and asserts the JSON report is well-formed and reflects reality.
//
// Covers the slice of Step 2 not exercised by adapter-level tests:
// the cobra command wiring, flag parsing, registry filter, and JSON
// report emission.
func TestInitDryRunJSONReportShape(t *testing.T) {
	// Save and restore mutable package-level init flags — tests that
	// run after this one would otherwise see leftover state.
	saved := struct {
		yes, dryRun, json bool
		agents            string
	}{initYes, initDryRun, initJSON, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON = saved.yes, saved.dryRun, saved.json
		initAgents = saved.agents
	})

	repo := t.TempDir()
	home := t.TempDir()
	isolateInitTestEnv(t, home)

	initYes = true
	initDryRun = true
	initJSON = true
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	// stdout must be a parseable JSON object.
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("non-JSON stdout: %v\n%s", err, stdout.String())
	}
	if report["dry_run"] != true {
		t.Fatalf("dry_run flag missing from report: %v", report)
	}
	rawAgents, ok := report["agents"].([]any)
	if !ok || len(rawAgents) != 1 {
		t.Fatalf("expected exactly 1 agent in report, got %v", report["agents"])
	}
	agent := rawAgents[0].(map[string]any)
	if agent["name"] != "claude-code" {
		t.Fatalf("expected claude-code, got %v", agent["name"])
	}
	files, ok := agent["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("expected non-empty files array, got %v", agent["files"])
	}
	for _, f := range files {
		fm := f.(map[string]any)
		action, _ := fm["action"].(string)
		if !strings.HasPrefix(action, "would-") && action != "skip" {
			t.Fatalf("dry-run emitted non-planning action %q for %v", action, fm["path"])
		}
	}

	// Dry-run must not have created anything on disk.
	for _, p := range []string{
		filepath.Join(repo, ".mcp.json"),
		filepath.Join(repo, "CLAUDE.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("dry-run wrote %s", p)
		}
	}
}

// TestInitAgentsFilterRejectsUnknownName is the hard-error contract
// for unknown --agents values — silent skips would mask typos.
func TestInitAgentsFilterRejectsUnknownName(t *testing.T) {
	saved := struct {
		yes, dryRun, json bool
		agents            string
	}{initYes, initDryRun, initJSON, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON = saved.yes, saved.dryRun, saved.json
		initAgents = saved.agents
	})

	repo := t.TempDir()
	isolateInitTestEnv(t, t.TempDir())

	initYes = true
	initDryRun = true
	initJSON = true
	initAgents = "claude-code,nonexistent-agent"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	err := runInit(initCmd, []string{repo})
	if err == nil {
		t.Fatal("expected error for unknown agent name, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-agent") {
		t.Fatalf("expected error to name the typo, got: %v", err)
	}
}

// TestInitCreatesProjectMarker pins the fix for issue #14: a fresh
// repo without `.gortex/` must end up with one after `gortex init` so
// the MCP server can resolve it as a single-project entry point.
func TestInitCreatesProjectMarker(t *testing.T) {
	saved := struct {
		yes, dryRun, json bool
		agents            string
	}{initYes, initDryRun, initJSON, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON = saved.yes, saved.dryRun, saved.json
		initAgents = saved.agents
	})

	inheritedConfig := t.TempDir()
	inheritedData := t.TempDir()
	inheritedCache := t.TempDir()
	inheritedDaemon := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", inheritedConfig)
	t.Setenv("XDG_DATA_HOME", inheritedData)
	t.Setenv("XDG_CACHE_HOME", inheritedCache)
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(inheritedDaemon, "daemon.sock"))
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(inheritedDaemon, "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_LOGFILE", filepath.Join(inheritedDaemon, "daemon.log"))
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(inheritedDaemon, "daemon.gob.gz"))

	repo := t.TempDir()
	isolateInitTestEnv(t, t.TempDir())

	initYes = true
	initDryRun = false
	initJSON = false
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	marker := filepath.Join(repo, ".gortex")
	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("expected %s to exist after init, got: %v", marker, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", marker)
	}

	// Idempotency: a second run must not error out.
	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	for _, root := range []string{inheritedConfig, inheritedData, inheritedCache, inheritedDaemon} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read inherited state root %s: %v", root, err)
		}
		if len(entries) != 0 {
			t.Fatalf("runInit wrote through inherited state root %s: %v", root, entries)
		}
	}
}

// TestInitDryRunSkipsProjectMarker pins the inverse: dry-run is a
// planning mode and must not write the marker either, otherwise it
// would silently bind the directory.
func TestInitDryRunSkipsProjectMarker(t *testing.T) {
	saved := struct {
		yes, dryRun, json, dryRunIntake bool
		agents                          string
	}{initYes, initDryRun, initJSON, initDryRunIntake, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON, initDryRunIntake = saved.yes, saved.dryRun, saved.json, saved.dryRunIntake
		initAgents = saved.agents
	})

	repo := t.TempDir()
	isolateInitTestEnv(t, t.TempDir())

	initYes = true
	initDryRun = true
	initJSON = true
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(repo, ".gortex")); err == nil {
		t.Fatal("dry-run wrote .gortex/ — must be planning-only")
	}
}

// TestInitRefusesHomeDirectory pins the safety guard in runInit:
// pointing gortex init directly at $HOME must be refused before any
// indexing or file writes happen, since that would crawl every
// project and dotfile under it.
func TestInitRefusesHomeDirectory(t *testing.T) {
	saved := struct {
		yes, dryRun, json, force bool
		agents                   string
	}{initYes, initDryRun, initJSON, initForce, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON, initForce = saved.yes, saved.dryRun, saved.json, saved.force
		initAgents = saved.agents
	})

	home := t.TempDir()
	isolateInitTestEnv(t, home)

	initYes = true
	initDryRun = false
	initJSON = false
	initForce = false
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	err := runInit(initCmd, []string{home})
	if err == nil {
		t.Fatal("expected runInit to refuse indexing $HOME, got nil error")
	}
	if !strings.Contains(err.Error(), "home directory") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("unexpected error message: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(home, ".gortex")); statErr == nil {
		t.Fatal("refused init still wrote .gortex/ into $HOME")
	}
}

// TestInitForceBypassesHomeDirectoryGuard confirms --force is the
// documented escape hatch for the guard above.
func TestInitForceBypassesHomeDirectoryGuard(t *testing.T) {
	saved := struct {
		yes, dryRun, json, force bool
		agents                   string
	}{initYes, initDryRun, initJSON, initForce, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON, initForce = saved.yes, saved.dryRun, saved.json, saved.force
		initAgents = saved.agents
	})

	home := t.TempDir()
	isolateInitTestEnv(t, home)

	initYes = true
	initDryRun = true // planning-only: keeps the forced run from writing into $HOME during the test
	initJSON = false
	initForce = true
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{home}); err != nil {
		t.Fatalf("runInit with --force: %v\nstderr: %s", err, stderr.String())
	}
}

func TestInitHooksOnlyRefreshesClaudeAndCodexHooks(t *testing.T) {
	restore := saveInitGlobals(t)
	defer restore()

	repo := t.TempDir()
	home := t.TempDir()
	isolateInitTestEnv(t, home)

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(codexDir, "config.toml")
	seed := `model = "gpt-5-codex"

[mcp_servers.gortex]
command = "custom-gortex"
args = ["mcp", "--custom"]
`
	if err := os.WriteFile(codexConfig, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	initYes = true
	initHooksOnly = true
	initHookMode = "enrich"
	initDryRun = false
	initJSON = false
	initAgents = ""

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err != nil {
		t.Fatalf("expected Claude Code hooks to be refreshed: %v", err)
	}
	cfg := readTOMLFile(t, codexConfig)
	if cfg["model"] != "gpt-5-codex" {
		t.Fatalf("Codex hooks-only clobbered model: %#v", cfg)
	}
	servers := cfg["mcp_servers"].(map[string]any)
	gortexServer := servers["gortex"].(map[string]any)
	if gortexServer["command"] != "custom-gortex" {
		t.Fatalf("Codex hooks-only rewrote mcp_servers.gortex: %#v", gortexServer)
	}
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("Codex hooks-only did not write hooks: %#v", cfg)
	}
	for _, event := range []string{"SessionStart", "PreToolUse", "PostToolUse"} {
		if _, ok := hooks[event]; !ok {
			t.Fatalf("Codex hooks-only missing %s hook: %#v", event, hooks)
		}
	}

	for _, p := range []string{
		filepath.Join(repo, ".gortex"),
		filepath.Join(repo, "AGENTS.md"),
		filepath.Join(repo, ".claude", "skills"),
		filepath.Join(home, ".gortex", "config.yaml"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("hooks-only should not create %s", p)
		}
	}
}

func TestInitHooksOnlyRespectsAgentsAllowlist(t *testing.T) {
	restore := saveInitGlobals(t)
	defer restore()

	repo := t.TempDir()
	home := t.TempDir()
	isolateInitTestEnv(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	initYes = true
	initHooksOnly = true
	initDryRun = false
	initJSON = false
	initAgents = "claude-code"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err != nil {
		t.Fatalf("expected Claude Code hooks to be refreshed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("--agents=claude-code should not write Codex config, stat err=%v", err)
	}
}

func TestInitHooksOnlyRespectsAgentsSkip(t *testing.T) {
	restore := saveInitGlobals(t)
	defer restore()

	repo := t.TempDir()
	home := t.TempDir()
	isolateInitTestEnv(t, home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexConfig := filepath.Join(codexDir, "config.toml")
	seed := `model = "gpt-5-codex"

[mcp_servers.gortex]
command = "custom-gortex"
`
	if err := os.WriteFile(codexConfig, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	initYes = true
	initHooksOnly = true
	initDryRun = false
	initJSON = false
	initAgentsSkip = "codex"

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(repo, ".claude", "settings.local.json")); err != nil {
		t.Fatalf("expected Claude Code hooks to be refreshed: %v", err)
	}
	cfg := readTOMLFile(t, codexConfig)
	if _, ok := cfg["hooks"]; ok {
		t.Fatalf("--agents-skip=codex should not write Codex hooks: %#v", cfg["hooks"])
	}
	if cfg["model"] != "gpt-5-codex" {
		t.Fatalf("Codex config was unexpectedly rewritten: %#v", cfg)
	}
}

func TestInitHooksOnlyDryRunDoesNotWriteHooks(t *testing.T) {
	restore := saveInitGlobals(t)
	defer restore()

	repo := t.TempDir()
	home := t.TempDir()
	isolateInitTestEnv(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}

	initYes = true
	initHooksOnly = true
	initDryRun = true
	initJSON = false
	initAgents = ""

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	for _, p := range []string{
		filepath.Join(repo, ".claude", "settings.local.json"),
		filepath.Join(home, ".codex", "config.toml"),
		filepath.Join(repo, ".gortex"),
		filepath.Join(repo, "AGENTS.md"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("dry-run hooks-only should not write %s", p)
		}
	}
}

func TestInitDryRunIntakeJSONDoesNotWrite(t *testing.T) {
	saved := struct {
		yes, dryRun, json, dryRunIntake bool
		agents                          string
	}{initYes, initDryRun, initJSON, initDryRunIntake, initAgents}
	t.Cleanup(func() {
		initYes, initDryRun, initJSON, initDryRunIntake = saved.yes, saved.dryRun, saved.json, saved.dryRunIntake
		initAgents = saved.agents
	})

	repo := t.TempDir()
	isolateInitTestEnv(t, t.TempDir())
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte(strings.Repeat("x", 32)), 0o644); err != nil {
		t.Fatal(err)
	}

	initYes = true
	initDryRunIntake = true
	initDryRun = false
	initJSON = false
	initAgents = ""

	var stdout, stderr bytes.Buffer
	initCmd.SetOut(&stdout)
	initCmd.SetErr(&stderr)
	t.Cleanup(func() {
		initCmd.SetOut(nil)
		initCmd.SetErr(nil)
	})

	if err := runInit(initCmd, []string{repo}); err != nil {
		t.Fatalf("runInit: %v\nstderr: %s", err, stderr.String())
	}

	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("non-JSON stdout: %v\n%s", err, stdout.String())
	}
	if report["schema_version"] != "gortex.index_intake.v1" {
		t.Fatalf("unexpected schema: %v", report["schema_version"])
	}
	if report["raw_paths_included"] != false || report["raw_file_contents_included"] != false {
		t.Fatalf("manifest must not include raw paths/content: %v", report)
	}
	if _, err := os.Stat(filepath.Join(repo, ".gortex")); err == nil {
		t.Fatal("dry-run-intake wrote .gortex/ — must be inspection-only")
	}
}

// isolateInitTestEnv prevents inherited machine state from escaping the test sandbox.
func isolateInitTestEnv(t *testing.T, home string) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "runtime"))
	t.Setenv("GORTEX_DAEMON_SOCKET", filepath.Join(root, "daemon.sock"))
	t.Setenv("GORTEX_DAEMON_PIDFILE", filepath.Join(root, "daemon.pid"))
	t.Setenv("GORTEX_DAEMON_LOGFILE", filepath.Join(root, "daemon.log"))
	t.Setenv("GORTEX_DAEMON_SNAPSHOT", filepath.Join(root, "daemon.gob.gz"))
}

func saveInitGlobals(t *testing.T) func() {
	t.Helper()
	saved := struct {
		analyze, installHooks, noHooks, hooksOnly bool
		hookMode                                  string
		skills, noSkills                          bool
		skillsMinSize, skillsMaxSkills            int
		yes, interactive                          bool
		agents, agentsSkip                        string
		json, dryRun, dryRunIntake, force         bool
	}{
		initAnalyze, initInstallHooks, initNoHooks, initHooksOnly,
		initHookMode,
		initSkills, initNoSkills,
		initSkillsMinSize, initSkillsMaxSkills,
		initYes, initInteractive,
		initAgents, initAgentsSkip,
		initJSON, initDryRun, initDryRunIntake, initForce,
	}
	return func() {
		initAnalyze, initInstallHooks, initNoHooks, initHooksOnly = saved.analyze, saved.installHooks, saved.noHooks, saved.hooksOnly
		initHookMode = saved.hookMode
		initSkills, initNoSkills = saved.skills, saved.noSkills
		initSkillsMinSize, initSkillsMaxSkills = saved.skillsMinSize, saved.skillsMaxSkills
		initYes, initInteractive = saved.yes, saved.interactive
		initAgents, initAgentsSkip = saved.agents, saved.agentsSkip
		initJSON, initDryRun, initDryRunIntake, initForce = saved.json, saved.dryRun, saved.dryRunIntake, saved.force
	}
}

func readTOMLFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return out
}
