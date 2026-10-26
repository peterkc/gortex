# Phase-1 driver review fixes

Source edits only. The lead owns Linux CI, shared bindings, checkout pins,
commits and whole-phase acceptance. No local tests, model work or daemon calls
ran. Gortex localization and recall remain unrun under the file-only Mac hold;
current source and the approved contract supply this report's evidence.

## Decisions and rejection behavior

- Storage dispatch priority is oracle, sqlite-segment, pebble-bleve,
  sqlite-vecflat, sqlite-usearch, catalog-sqlite, catalog-refs, then the remaining
  SCREEN candidates in their existing order. This is the lead's B5 decision,
  not a duration prediction or permission to omit the rest. The README's
  concurrency 1 is the serial invocation. Approved 2/3 still use memory
  admission; no timing block becomes concurrent.
- Each storage/timing job may use the remaining shared block window minus its
  cleanup reserve. The inventory's 150/120 minutes are maxima sharing one
  block, not twelve independent allocations. No absolute window is enlarged.
- Timing starts every eligible finalist/tier's first repeat before any second
  repeat. Timing/catalog/hybrid need a completed passing oracle, not a complete
  screen. A failed oracle blocks those dependents.
- Clean block-local errors remain partial after verified group teardown. This
  includes missing/malformed receipts, exit 2, a partial runner receipt and a
  stopped storage verdict. FatalSession marks verified process-identity,
  surviving-member, live-termination and shared-memory failures at their owner.
  Signals and unclean receipts still stop the session. CPU dependencies retain
  the approved block_limit conditions; incomplete storage/model work can still
  block CPU, without blocking the A7 attempt.
- Prepare copies evaluator/CPU inputs and the three pinned control folders.
  Existing destinations, symlinks and special files refuse; copied bytes are
  checked and every copied file enters setup-complete artifacts. Missing input
  folders name the rejected source. lost+found is tolerated.
- Allocation timestamps normalize trailing Z and fractional seconds to the
  Python 3.10 datetime microsecond precision. The full enforcement invocation
  records root, inputs and selected concurrency. Execution refuses effective
  UID 0. Partial-storage resume refuses missing receipts without replay.
- The subreaper collects previously unseen adopted zombies. A direct-child exit
  racing with poll retains its actual wait status. The final A9 receipt cannot
  claim a clean handoff after an uncaught error or failed cleanup.

## Regression cases written, not run

- `test_block_local_receipt_failures_release_the_next_block`
- `test_unclean_receipt_and_survivor_stop_the_whole_session`
- `test_oracle_success_releases_passed_timing_after_screen_timeout`
- `test_priority_repeat_rounds_and_remaining_window_limits`
- `test_staging_hashes_and_missing_existing_or_symlink_rejection`
- `test_timestamp_normalization_and_full_enforcement_invocation`
- `test_reap_unknown_zombie_preserves_direct_child_status`
- `test_partial_storage_resume_refuses_missing_receipts_cleanly`
- `test_root_user_refuses_before_host_or_process_effects`
- `test_uncaught_error_cannot_claim_a_clean_a9_handoff`

No existing test was deleted. The package-refusal fixture now stages the two
required input folders and lost+found, preserving its original rejection check.
The existing test files and supervisor fixtures are the local precedents.

## Python 3.10 Linux CI command

Run from the checkout root. It uses only an already-cached interpreter and
fails visibly if absent. It downloads nothing.

```sh
python3 -c 'import os, pathlib
paths = sorted(pathlib.Path(os.environ["RUNNER_TOOL_CACHE"]).glob("Python/3.10.*/x64/bin/python3.10"))
if not paths: raise SystemExit("FAIL: Python 3.10 absent from RUNNER_TOOL_CACHE; no download allowed")
p = str(paths[0])
os.execv(p, [p, "-m", "unittest", "discover", "-s", "study/storage/pod", "-p", "test_driver.py", "-v"])'
```

## Source-only verification

- Production diff: 133 added + 58 removed = 191 changed lines against 792c1a97.
- Driver: 2,322 lines. Tests: 1,167 lines, 33 methods (ten new; 23 retained).
- All four changed Python files passed Python 3.11.15 py_compile, exit 0,
  empty output. This does not prove Python 3.10 or runtime behavior.
- Driver/test grammar also parses at feature_version 3.10. The source audit
  found no 3.11-only import/API in those two files. Python 3.10 CI is unrun.
- No existing test was deleted. No whitespace-only replacement was found in
  the changed Python files. No repository formatter or linter was introduced.
- Driver source SHA-256: `d7c0a2d4a3625378299064a376f13557f649b64146bc88b421bfd26ce7468e3b`.
- Shared bindings, workflow, immutable checkout pin and acceptance remain
  lead-owned. The accepted pin must include these copied control folders.
- The seven-lines-per-side numstat/-w gap is the required A9 body dedent when
  removing its old conditional write, not an unrelated file restyle.
