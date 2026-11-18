# Telemetry & privacy

Gortex can collect **anonymous usage statistics** — coarse, bucketed counts of *which* tools and commands run. That anonymous rollup is **opt-in and OFF by default**: nothing is buffered or sent until you explicitly enable it, and even when enabled nothing is transmitted unless an ingest endpoint is configured. Separate, non-transmitted hook diagnostics may be written locally as described below. Neither the rollup nor hook diagnostics record code, file paths, file names, symbol names, or repository names. The optional ONNX Runtime backend has separate telemetry controls and data, described below.

## Quick control

```bash
gortex telemetry status   # is it on/off, why, what is collected, the anonymous install id
gortex telemetry on       # enable anonymous tool/command counts
gortex telemetry off      # disable and delete any buffered, unsent data
```

- `telemetry on` prints `Telemetry enabled — anonymous tool/command counts only (no code, paths, or names).`
- `telemetry off` prints `Telemetry disabled — buffered data cleared.` — it deletes all buffered daily rollups and the send marker (the anonymous install id is intentionally kept).
- `telemetry status` prints the state and the precedence rung that decided it (`decided by: env | do_not_track | config | default`), the ingest endpoint (`not configured — nothing is transmitted` when unset), the anonymous install id, and a one-line summary of what is collected.

`gortex install` also offers the choice: pass `--telemetry` / `--no-telemetry`, or use the **Anonymous telemetry** toggle in the interactive wizard. A toggle takes effect on a running daemon within a few seconds (it re-reads consent on the record and send paths) — no restart needed.

## How consent is decided

Consent resolves through a fixed four-rung precedence (**highest wins**):

| Rung | Signal | Effect |
| --- | --- | --- |
| 1 | `GORTEX_TELEMETRY` | Explicit per-process override — can force **on or off**. ON values `1/true/on/yes/enable/enabled`; OFF values `0/false/off/no/disable/disabled` (case-insensitive, trimmed). Highest, so it overrides even a global `DO_NOT_TRACK` for one invocation. |
| 2 | `DO_NOT_TRACK` | The cross-tool standard ([consoledonottrack.com](https://consoledonottrack.com)). Any set value other than `0`/`false` forces **off**. Can only ever disable, never enable. |
| 3 | Saved choice | The persisted `telemetry.enabled` value from `gortex telemetry on/off`. |
| 4 | Default | **Off.** |

An unrecognised or empty value at a rung falls through to the next rung rather than being treated as a decision. To turn this rollup off, set `DO_NOT_TRACK=1` or `GORTEX_TELEMETRY=0`.

## ONNX Runtime telemetry

The optional `embeddings_onnx` backend loads a user-installed ONNX Runtime library. Official Runtime builds from 1.29 onward include Microsoft usage telemetry on Linux and macOS. It can include a hashed device identifier, the process launch path, CPU and OS information, and model names. This is separate from Gortex's anonymous rollup and its no-paths guarantee.

Gortex keeps ONNX Runtime telemetry **off by default** by setting `ORT_DISABLE_TELEMETRY=1` before loading the library and calling its telemetry-off API after environment creation. A sandboxed Runtime 1.31.0 check created no device-id or queue files by default; the opt-in control created both. To permit it, set `GORTEX_ONNX_TELEMETRY=1` in the daemon's environment before starting it. Permission does not force telemetry on: an inherited `ORT_DISABLE_TELEMETRY=1` still wins because Gortex leaves that variable unchanged when you opt in.

For this backend, `DO_NOT_TRACK` forces telemetry off even with `GORTEX_ONNX_TELEMETRY=1`: after trimming whitespace and ignoring case, any value other than empty, `0`, or `false` asserts it. `GORTEX_TELEMETRY=0` and `gortex telemetry off` control only Gortex's anonymous rollup; they do **not** cover ONNX Runtime. The default pure-Go backends do not load ONNX Runtime. See [Semantic search](semantic-search.md) for backend setup.

## What is collected

Only the following metric keys can ever be recorded — a hard allow-list; the aggregator physically cannot record anything else:

| Key | Meaning | Dimension |
| --- | --- | --- |
| `mcp_tool_call` | an MCP tool was invoked | tool name (e.g. `search_symbols`) |
| `mcp_facade_call` | a compact-surface operation was attempted | registered `facade.operation` or a fixed `unknown` sentinel |
| `mcp_facade_status` | coarse facade result status | registered `facade.operation.ok|error` |
| `mcp_facade_outcome` | bounded facade result class | registered `facade.operation` plus `success`, `invalid_operation`, `invalid_argument`, `blocked`, `unavailable`, `tool_error`, `handler_error`, or `empty_result` |
| `mcp_facade_invalid` | facade validation failed | registered `facade.operation.invalid_argument` |
| `mcp_facade_latency` | facade end-to-end latency | registered `facade.operation` plus a duration bucket |
| `cli_command` | a CLI subcommand ran | dotted command path (e.g. `daemon.start`, `review`) |
| `index` | an index pass completed | file-count bucket |
| `index_lang` | a language occurred in an index pass | language name from the fixed parser registry |
| `daemon_session` | a daemon session started | backend kind (e.g. `sqlite`) |
| `install` | an agent integration was installed | fixed agent target/scope |
| `uninstall` | an agent integration was removed | fixed agent target/scope |
| `client` | an MCP client connected | bounded client family |

A recorded counter is `key` or `key:dimension` (e.g. `mcp_tool_call:search_symbols`, `index:1k-10k`). Values are **bucketed**, never exact — exact counts can narrow identification:

- File counts → `<100`, `100-1k`, `1k-10k`, `10k+`
- Facade latencies → `<1ms`, `1-10ms`, `10-100ms`, `100ms-1s`, `1-10s`, `10s+`

Facade dimensions are admitted only from the canonical operation registry (or fixed sentinels). Caller-provided operation/domain values never become a metric dimension, even in hashed form.

### Local hook-effectiveness diagnostics

Hooks also keep a local, non-transmitted JSONL diagnostic at
`~/.gortex/cache/hook-effectiveness.jsonl` (override with
`GORTEX_HOOK_EFFECTIVENESS_LOG`). One bounded record is written for every
Claude-compatible or Codex event handled by the shared hook dispatcher,
including no-op events, so the denominator cannot disappear.
Each record contains only:

- an allow-listed event name;
- whether the hook emitted model-visible context or a decision reason;
- whether the local daemon was reachable;
- a capped alternation-segment count (`0` when not applicable); and
- hook latency in milliseconds.

Commands, grep patterns, prompts, source, paths, symbols, repositories, and
tool output are never written to this log. This diagnostic is separate from
the opt-in anonymous rollup above and is never uploaded. Together with the
existing local `hook-decisions.jsonl`, it directly exposes emitted-context
rate and makes another high-skip regression visible.

A dimension guard (`^[A-Za-z0-9_.<>+-]{1,32}$`) drops any token containing a path separator, whitespace, or over 32 characters, so even a caller that mistakenly passed a path or symbol name as a dimension cannot leak it — only the bare metric key is recorded.

### Never collected by Gortex's anonymous rollup

Code or source content; file paths or names; symbol or repository names; IP-derived data; hostnames, usernames, or MAC addresses; exact counts.

## The anonymous install id

A random UUIDv4 minted on first use and stored at `~/.gortex/telemetry/install-id`. It is the only stable identifier telemetry carries, derived from nothing about your machine (no hostname, user, MAC, or path) — it merely ties one machine's daily aggregates together. `gortex telemetry status` prints it.

## Where state lives

All state for Gortex's anonymous rollup is under `~/.gortex/telemetry/` (`<data-dir>/telemetry`; honours XDG / data-dir overrides). It lives under the durable data dir, not the cache, so a cache wipe never resets the install id or drops unsent days. Contents:

- per-UTC-day rollup files — the buffered daily aggregates
- `install-id` — the anonymous id
- `consent.json` — the persisted choice
- `last-send` — the once-per-day send marker

## Transmission

There is **no built-in default endpoint**. With `GORTEX_TELEMETRY_ENDPOINT` unset, telemetry is aggregated locally but **never transmitted** — the whole pipeline runs end-to-end except the final POST. When the endpoint is configured, and only while consent is enabled, Gortex sends:

- a single JSON `POST` to the endpoint, 5s timeout, **no retries** (a failed send leaves the days buffered)
- **at most once per UTC machine-day**
- only **completed** UTC days — never the day still accumulating; on a successful (`2xx`) response the sent days are deleted

Payload shape:

```json
{ "install_id": "…", "schema_version": 1, "gortex_version": "…",
  "os": "darwin", "arch": "arm64", "ci": false,
  "days": [ { "day": "2026-06-18", "counts": { "cli_command:review": 3, "index:1k-10k": 1 } } ] }
```

`os`/`arch` are the Go `runtime.GOOS`/`GOARCH`; `ci` reflects whether a CI environment was detected (`CI` env var set and not `0`/`false`); `counts` holds only the allow-listed, bucketed keys.

## First-run notice

The first time you run `gortex init` (non-dry-run), Gortex prints a one-time notice to stderr and records the default (off) choice so the notice fires at most once. It never enables anything:

> Gortex can collect anonymous usage stats (tool/command counts only — no code, paths, or names). It is OFF by default; enable with `gortex telemetry on`. See `gortex telemetry status`.
