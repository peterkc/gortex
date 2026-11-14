package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"github.com/zzet/gortex/internal/persistence"
	"github.com/zzet/gortex/internal/platform"
	"gopkg.in/yaml.v3"
)

var (
	toolsReceiptFormat string
	toolsReceiptSince  time.Duration
)

var toolsReceiptCmd = &cobra.Command{
	Use:   "receipt",
	Short: "Print a context-budget receipt: the MCP tool surface the CLI path did NOT mount",
	Long: `Emits an inspectable "context budget receipt" describing the transport and
tool-surface counts the daemon would advertise over MCP. Driving Gortex through
the CLI (or a skill that shells the CLI) mounts no tool schemas into the model's
context, so this receipt is the auditable record of the per-call "tax" avoided.

When a daemon tracks the repo the receipt reports its active preset and the
live / deferred tool counts; when no daemon is reachable it still emits a
receipt recording that no surface was mounted.

The receipt also reports the CLI verbs the current agent session drove and the
safety steps those verbs ran. Capture is per-session and opt-in: set
GORTEX_SESSION_ID before invoking gortex so each verb is recorded under that
correlation id; the receipt then reads back exactly this session's verbs. With
no session id set the receipt falls back to verbs run within the --since window
(across all sessions), and those two fields are empty when nothing was
recorded.`,
	RunE: runToolsReceipt,
}

func init() {
	toolsReceiptCmd.Flags().StringVar(&toolsReceiptFormat, "format", "yaml", "output format: yaml or json")
	toolsReceiptCmd.Flags().DurationVar(&toolsReceiptSince, "since", time.Hour, "when GORTEX_SESSION_ID is unset, include CLI verbs run within this window")
	toolsCmd.AddCommand(toolsReceiptCmd)
}

// receiptSidecarPath resolves the sidecar database the CLI-verb ledger is read
// from for Part B. A package var so tests can redirect it away from the real
// ~/.gortex. Mirrors cliEventSidecarPath (the write side in root.go).
var receiptSidecarPath = func() string {
	return persistence.DefaultSidecarPath(platform.DataDir())
}

// receiptNow is the clock the receipt uses to window CLIEventsSince. A package
// var so tests can pin it.
var receiptNow = time.Now

// getenvReceipt is the os.Getenv seam the receipt reads through, so tests can
// stub the session-id lookup without mutating the process environment.
var getenvReceipt = os.Getenv

// receiptSessionID reads the per-session correlation id the receipt scopes its
// CLI-verb read by. A seam (over os.Getenv) so tests can drive it.
var receiptSessionID = func() string { return getenvReceipt("GORTEX_SESSION_ID") }

// verbSafetyTool maps a CLI verb (cliCommandDim form, e.g. "edit.verify") to
// the MCP safety tool it corresponds to. The receipt derives safety_steps_run
// from the recorded verbs through this map — only the documented safety steps
// of the edit/verify cycle are mapped; every other verb (query.stats, etc.)
// maps to nothing.
var verbSafetyTool = map[string]string{
	"edit.verify":     "verify_change",
	"edit.guards":     "check_guards",
	"edit.tests":      "get_test_targets",
	"edit.contract":   "change_contract",
	"feedback.record": "feedback",
	"feedback.query":  "feedback",
}

// safetyStepsForVerbs returns the DISTINCT safety tools the given verbs ran,
// via verbSafetyTool, in a stable (sorted) order. Verbs with no safety mapping
// contribute nothing.
func safetyStepsForVerbs(verbs []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, v := range verbs {
		tool, ok := verbSafetyTool[v]
		if !ok {
			continue
		}
		if _, dup := seen[tool]; dup {
			continue
		}
		seen[tool] = struct{}{}
		out = append(out, tool)
	}
	sort.Strings(out)
	return out
}

// toolProfileCounts is the slice of the daemon's tool_profile response the
// receipt reads: the active preset and the live / deferred surface counts.
type toolProfileCounts struct {
	Preset        string   `json:"preset"`
	PresetMode    string   `json:"preset_mode"`
	LiveCount     int      `json:"live_count"`
	DeferredCount int      `json:"deferred_count"`
	Live          []string `json:"live"`
	Deferred      []string `json:"deferred"`
}

// contextBudgetReceipt is the rendered receipt body. Field order is fixed via
// struct order so both the YAML and JSON renderings are deterministic.
type contextBudgetReceipt struct {
	Transport             string                      `json:"transport" yaml:"transport"`
	Repo                  string                      `json:"repo" yaml:"repo"`
	DaemonPreset          string                      `json:"daemon_preset,omitempty" yaml:"daemon_preset,omitempty"`
	DaemonPresetMode      string                      `json:"daemon_preset_mode,omitempty" yaml:"daemon_preset_mode,omitempty"`
	AdvertisedTools       int                         `json:"advertised_tools" yaml:"advertised_tools"`
	RegisteredToolSchemas int                         `json:"registered_tool_schemas" yaml:"registered_tool_schemas"`
	DeferredCapabilities  receiptDeferredCapabilities `json:"deferred_capabilities" yaml:"deferred_capabilities"`
	Decision              string                      `json:"decision" yaml:"decision"`
	// Part B: the CLI verbs the current agent session drove and the safety
	// steps derived from them. Always present (empty lists when nothing was
	// recorded) so the receipt shape is stable. Note explains how to enable
	// per-session capture when it is empty.
	CLIVerbsUsed   []string `json:"cli_verbs_used" yaml:"cli_verbs_used"`
	SafetyStepsRun []string `json:"safety_steps_run" yaml:"safety_steps_run"`
	Note           string   `json:"note,omitempty" yaml:"note,omitempty"`
}

type receiptDeferredCapabilities struct {
	Count int `json:"count" yaml:"count"`
}

// receiptEnvelope wraps the receipt body under the single top-level key both
// renderings carry.
type receiptEnvelope struct {
	GortexContextBudget contextBudgetReceipt `json:"gortex_context_budget" yaml:"gortex_context_budget"`
}

func runToolsReceipt(cmd *cobra.Command, _ []string) error {
	abs, err := filepath.Abs(toolsIndex)
	if err != nil {
		abs = toolsIndex
	}

	receipt := buildContextBudgetReceipt(abs)
	return renderReceipt(cmd, receipt, toolsReceiptFormat)
}

// buildContextBudgetReceipt asks the daemon for its tool_profile and renders
// the receipt. Any error reaching the daemon (no daemon running, repo not
// tracked, or a tool failure) is treated as "no surface mounted" rather than a
// hard failure — the receipt must be emittable even with nothing mounted.
func buildContextBudgetReceipt(absRepo string) contextBudgetReceipt {
	var receipt contextBudgetReceipt
	raw, err := toolsDaemonTool(toolsIndex, "tool_profile", map[string]any{})
	if err != nil {
		receipt = contextBudgetReceipt{
			Transport:             "cli_only",
			Repo:                  absRepo,
			AdvertisedTools:       0,
			RegisteredToolSchemas: 0,
			DeferredCapabilities:  receiptDeferredCapabilities{Count: 0},
			Decision:              "no_surface_mounted",
		}
	} else {
		var counts toolProfileCounts
		_ = json.Unmarshal(raw, &counts)

		// Prefer the explicit *_count fields; fall back to the array lengths so a
		// profile shape that omits the counts still yields a faithful receipt.
		live := counts.LiveCount
		if live == 0 && len(counts.Live) > 0 {
			live = len(counts.Live)
		}
		deferred := counts.DeferredCount
		if deferred == 0 && len(counts.Deferred) > 0 {
			deferred = len(counts.Deferred)
		}

		receipt = contextBudgetReceipt{
			Transport:             "skill_cli",
			Repo:                  absRepo,
			DaemonPreset:          counts.Preset,
			DaemonPresetMode:      counts.PresetMode,
			AdvertisedTools:       live,
			RegisteredToolSchemas: 0,
			DeferredCapabilities:  receiptDeferredCapabilities{Count: deferred},
			Decision:              "full_surface_not_mounted",
		}
	}

	attachReceiptPartB(&receipt)
	return receipt
}

// attachReceiptPartB fills cli_verbs_used / safety_steps_run from the
// consent-free CLI-verb ledger. The read is best-effort: if the sidecar can't
// be opened the two fields stay empty lists and Part A is emitted unchanged —
// never a hard error.
//
// Scope: with GORTEX_SESSION_ID set, exactly that session's verbs (the precise
// per-session capture); otherwise the verbs run within the --since window
// across all sessions. The verbs are de-duplicated in first-seen order;
// safety_steps_run is derived from them through verbSafetyTool.
func attachReceiptPartB(receipt *contextBudgetReceipt) {
	// Empty (never nil) so both renderings show an explicit empty list.
	receipt.CLIVerbsUsed = []string{}
	receipt.SafetyStepsRun = []string{}

	sessionID := receiptSessionID()
	if sessionID == "" {
		receipt.Note = "set GORTEX_SESSION_ID to capture this session's CLI verbs and safety steps"
	}

	native, err := persistence.OpenSidecar(receiptSidecarPath())
	if err != nil || native == nil {
		return
	}
	var sc cliEventReader = native
	defer func() { _ = sc.Close() }()

	var events []persistence.CLIEvent
	if sessionID != "" {
		events, err = sc.CLIEventsBySession(sessionID)
	} else {
		events, err = sc.CLIEventsSince(receiptNow().Add(-toolsReceiptSince))
	}
	if err != nil {
		return
	}

	verbs := distinctVerbs(events)
	if len(verbs) > 0 {
		receipt.CLIVerbsUsed = verbs
	}
	if steps := safetyStepsForVerbs(verbs); len(steps) > 0 {
		receipt.SafetyStepsRun = steps
	}
}

// distinctVerbs returns the distinct verbs from the events in first-seen
// (chronological) order — the events arrive oldest-first from the ledger.
func distinctVerbs(events []persistence.CLIEvent) []string {
	seen := make(map[string]struct{}, len(events))
	out := make([]string, 0, len(events))
	for _, ev := range events {
		if ev.Verb == "" {
			continue
		}
		if _, dup := seen[ev.Verb]; dup {
			continue
		}
		seen[ev.Verb] = struct{}{}
		out = append(out, ev.Verb)
	}
	return out
}

// renderReceipt writes the receipt as YAML (default) or JSON under the
// gortex_context_budget top-level key.
func renderReceipt(cmd *cobra.Command, receipt contextBudgetReceipt, format string) error {
	out := cmd.OutOrStdout()
	env := receiptEnvelope{GortexContextBudget: receipt}
	switch format {
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(env)
	case "yaml", "":
		b, err := yaml.Marshal(env)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(out, string(b))
		return err
	default:
		return fmt.Errorf("unknown --format %q (want yaml or json)", format)
	}
}
