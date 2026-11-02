# Frozen token-cut accuracy inputs and process limits

This is study tooling, not a product indexing path. It makes no inference or
accuracy claim. Both cuts must use the exact same frozen corpus bytes.

## Corpus build

`accuracy-sources-v1.json` pins Go `go1.23.2` and CPython `v3.12.7` by full
commit and Git tree. `fetch_accuracy_inputs.py` creates fresh checkouts without
submodules. It checks both identities before the builder can read them.
It also fetches exactly five CodeRankEmbed tokenizer/config files at revision
`3c4b60807d71f79b43f3c4363786d9493691f8b1`. It reads their expected hashes from
`../pod-cost/cpu-cost-v1.json`. It downloads no model weights and uses no model
cache or authentication.

`build_accuracy_corpus.py` reads only blobs enumerated from those Git trees.
It ignores working-file modifications. It rejects linked selected files and
noncanonical paths. Go declarations come from the stdlib-only `corpus_extract`
helper. Python declarations come from `ast` and `tokenize`. Nested definitions
remain in their enclosing bodies, not as separate candidates. Comments and
Python docstrings are blanked, but ordinary strings remain.
Go `*_test.go`, `src/vendor/` and `testdata/` files are excluded, as are
Python `Lib/test/` files. Go documentation follows `ast.CommentGroup.Text()`:
`//go:`, `//line` and `//export` directives never become query text. All comments,
including directives, remain in the source/license provenance.
Python spans and notices use UTF-8 byte offsets and LF-only physical lines.

Every model-ready body includes its kind, qualified name, signature and path.
Token counts include this header and special tokens, without truncation.
Queries use the first documentation paragraph, 30–400 Unicode characters, with
at most 512 tokens including the fixed query prefix and special tokens.
Exact duplicate queries keep the first canonical parent ID before selection.
No relevance classifier, paraphraser, vector feedback or scorer selects data.

The manifest fixes these language-specific quotas:

| Language | Complete body tokens | Documents | Labelled queries |
|----------|----------------------|-----------|------------------|
| Go       | 1–2,048              | 450       | 25               |
| Go       | 2,049–4,096          | 40        | 20               |
| Go       | 4,097–8,192          | 10        | 5                |
| Python   | 1–2,048              | 450       | 25               |
| Python   | 2,049–4,096          | 8         | 4                |
| Python   | 4,097–8,192          | 0         | 0                |

These quotas require **958 documents and 79 labelled queries**.
The pinned CPython population has only eight bodies above 2,048 tokens and none
above 4,096. Python is a short-code control; Go carries the long-body comparison.
Label every per-language Python result **Python short-code control**. Do not
claim Python long-body coverage. Population and above-cap counts remain in the
report even where a quota is zero.

Bodies above 8,192 are excluded, but their population count is reported.
Query targets are selected first by SHA-256 of language, bucket and parent ID.
Other bodies fill the same bucket in that order. A shortfall fails with
`counts.json` and `diagnostics.json`; there is no padding or smaller denominator.
Malformed source also fails, rather than being skipped or reinterpreted.

The CLI requires `--manifest`, its `--manifest-pin`, fresh pinned `--sources`,
local `--tokenizer`, the built `--go-extractor` and a fresh `--output` directory.
Run it with the frozen `../pod-cost` Python environment. Before loading the local
tokenizer, the builder sets `HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1`; it
requires `local_files_only=True` and disables remote code. CI builds and tests the
helper and builder, then runs the real build under a hard limit. CI uploads the
JSONL files, provenance, counts, complete repository licenses, selected-file
preambles and all comments/docstrings, source/license manifest and freeze
receipts. Review per-file notices before use; this tool does not classify legal
conflicts.

`freeze-canonical.json` contains the exact canonical bytes hashed as `freeze_id`.
`freeze.json` records that ID and all bindings. Output file hashes cover exact
UTF-8 JSONL bytes, final LF, ordered IDs, provenance, counts and licenses.
Source commits/trees, LICENSE SHA-256 values, tokenizer/config hashes, builder,
extractor, harness revision, full `sys.version` and frozen environment hashes
are bound too. JSONL escapes U+0085, U+2028 and U+2029, so line-oriented consumers
cannot split one record at those characters. Decoded text is unchanged.
Existing output, including an interrupted attempt, is never overwritten or
resumed.

## External process launcher

Build `../harness/cmd/limited-command` in the harness module. Invoke:

```text
limited-command -binding BINDING.json -binding-pin SHA256 -policy POLICY.json \
  -data FREEZE.json -cgroup-root DELEGATED_ROOT -out FRESH_LOG_DIRECTORY \
  -- /absolute/command arg ...
```

The existing `RunBinding` binds the launcher binary, exact child argv, host,
source revision, data receipt and policy hashes. Its `engine` limit applies to
the external process tree. Its `runner` and `checker` fields remain required by
the existing schema but do not describe processes launched here. The independent
small cleanup owner stays outside the target scope; the operator must bound that
owner separately. This launcher does not claim to do so.

`POLICY.json` has only `schema: "external-limits/v1"` and `deadline`, a future
absolute RFC3339 timestamp. No relative-time refresh is allowed. The launcher
refuses without writable delegated Linux cgroup v2 and working cgroup-fd child
placement. It reads back `memory.max`, `memory.swap.max=0` and
`memory.oom.group=1` before starting the command. It also requires writable
`cgroup.kill` before starting a child and bounds post-cancellation waiting with
the harness safety timeout. No systemd is required.

The child receives only the following environment variables, when present:

- Paths and locale: `PATH`, `HOME`, `TMPDIR`, `XDG_CONFIG_HOME`, `XDG_CACHE_HOME`,
  `XDG_DATA_HOME`, `XDG_STATE_HOME`, `XDG_RUNTIME_DIR`, `LANG`, `LANGUAGE`,
  `LC_ALL`, `LC_CTYPE`, `LC_COLLATE`, `LC_MESSAGES`, `LC_NUMERIC`, `LC_MONETARY`,
  `LC_TIME`, `TZ`.
- Go: `GOTOOLCHAIN`, `GOWORK`, `GOFLAGS`, `GOMAXPROCS`, `GOMEMLIMIT`, `GOOS`,
  `GOARCH`, `CGO_ENABLED`, `CGO_CFLAGS`, `CGO_CXXFLAGS`, `GOCACHE`, `GOMODCACHE`,
  `GOPATH`, `GOPROXY`, `GOSUMDB`, `GONOSUMDB`, `GOPRIVATE`, `GOINSECURE`,
  `GOENV`, `GOTMPDIR`.
- Python and inference: `PYTHONDONTWRITEBYTECODE`, `UV_PROJECT_ENVIRONMENT`,
  `UV_CACHE_DIR`, `UV_PYTHON`, `UV_PYTHON_DOWNLOADS`, `OMP_NUM_THREADS`,
  `OPENBLAS_NUM_THREADS`, `MKL_NUM_THREADS`, `NUMEXPR_NUM_THREADS`,
  `LD_LIBRARY_PATH`, `USE_TORCH`, `TOKENIZERS_PARALLELISM`, `HF_HUB_OFFLINE`,
  `TRANSFORMERS_OFFLINE`.

Other names are dropped, including `GITHUB_*`, `RUNNER_*`, `OUT`, Prime/RLM
selectors and credential variables such as `HF_TOKEN` and `AWS_SECRET_ACCESS_KEY`.
Pass command paths and options in argv. Environment
entries are sorted; a duplicate name keeps its last value. The receipt binds
`environment_sha256` to the UTF-8 `name=value` entries joined with NUL and one
final NUL. It does not record their values.

Use this launcher separately for prepare, body inference, query inference,
assembly and evaluation, with each approved role limit and deadline. A launch
returns nonzero on OOM, deadline, cancellation, child failure or cleanup failure.
It retains `command.log` and `receipt.json`, including peak bytes, OOM count,
exit code, cgroup path and removal result. Cleanup kills all remaining
owned descendants even after a normal leader exit, waits for an empty group,
and removes only that group. Never accept a partial cut or automatically retry
an unsuccessful attempt.
