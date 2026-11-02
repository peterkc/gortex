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

Every model-ready body includes its kind, qualified name, signature and path.
Token counts include this header and special tokens, without truncation.
Queries use the first documentation paragraph, 30–400 Unicode characters, with
at most 512 tokens including the fixed query prefix and special tokens.
Exact duplicate queries keep the first canonical parent ID before selection.
No relevance classifier, paraphraser, vector feedback or scorer selects data.

Each language has these fixed quotas:

| Complete body tokens | Documents | Labelled queries |
|----------------------|-----------|------------------|
| 1–2,048              | 450       | 25               |
| 2,049–4,096          | 40        | 20               |
| 4,097–8,192          | 10        | 5                |

Bodies above 8,192 are excluded, but their population count is reported.
Query targets are selected first by SHA-256 of language, bucket and parent ID.
Other bodies fill the same bucket in that order. A shortfall fails with
`counts.json` and `diagnostics.json`; there is no padding or smaller denominator.
Malformed source also fails, rather than being skipped or reinterpreted.

The CLI requires `--manifest`, its `--manifest-pin`, fresh pinned `--sources`,
local `--tokenizer`, the built `--go-extractor` and a fresh `--output` directory.
Run it with the frozen `../pod-cost` Python environment. CI builds and tests the
helper and builder, then runs the real build under a hard limit. CI uploads the
JSONL files, provenance, counts, complete repository licenses, selected-file
preambles and all comments/docstrings, source/license manifest and freeze
receipts. Review per-file notices before use; this tool does not classify legal
conflicts.

`freeze-canonical.json` contains the exact canonical bytes hashed as `freeze_id`.
`freeze.json` records that ID and all bindings. Output file hashes cover exact
UTF-8 JSONL bytes, final LF, ordered IDs, provenance, counts and licenses.
Source commits/trees, LICENSE SHA-256 values, tokenizer/config hashes, builder,
extractor, harness revision and frozen environment hashes are bound too.
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
`memory.oom.group=1` before starting the command. No systemd is required.

Use this launcher separately for prepare, body inference, query inference,
assembly and evaluation, with each approved role limit and deadline. A launch
returns nonzero on OOM, deadline, cancellation, child failure or cleanup failure.
It retains `command.log` and `receipt.json`, including peak bytes, OOM count,
exit code, cgroup path and removal result. Cleanup kills all remaining
owned descendants even after a normal leader exit, waits for an empty group,
and removes only that group. Never accept a partial cut or automatically retry
an unsuccessful attempt.
