# GCE driver adaptation (2026-10-27)

Source preparation only. This section supersedes the earlier Prime/launchd
references retained below as historical phase-1 evidence. The lead owns the
amended-plan approval, fork CI, commit/pin and VM admission. This task made no VM or cloud API calls.

## Current contract

- The procedure requires pi-gcp, GCE max-run-duration DELETE, provider TTL,
  TERMINATE maintenance, no VM credentials and exactly 710 duration minutes.
  Backstop minutes remain finite and within 690..720. Unknown fields reject.
- Allocation has exactly name, zone, project, id and created_at. The normalized
  receipt has exactly 24 fields, including provider duration seconds, boot disk
  name/type/size and source image. It binds the VM identity and creation time.
- Project is peterkc-dev. Status is RUNNING. Provisioning is STANDARD. The boot
  disk is 700 GB pd-balanced with auto-delete. Service accounts are an empty
  list. Login/UID/GPU and the firewall rule remain recorded; no GPU name pin
  or cloud/SSH operation is added. The image must contain the approved family
  name common-cu129-ubuntu-2204-nvidia-580, including resolved image names.
- Whole-second aware timestamps use parse_time. TTL equals last_start_time
  plus 710 minutes within 60 seconds. The absolute termination interval is
  created_at + 710:00 through +719:30, inclusive. Start later than +9:30 also
  rejects: otherwise the 60-second tolerance could conceal a late start.
- live_termination takes the validated procedure explicitly. The supervisor
  copies it and rechecks the live receipt before each spawn. Its deadline is
  the earlier of created_at + backstop_minute and provider termination_time.
  Every block respects that bound; the session's 720-minute ceiling remains.
- A9 handoffs record termination_time, not the removed deadline_pdt field.
  Launchd, plist/calendar, awake/caffeinate and Prime CLI requirements are gone.
  The server TTL, DELETE action, disk auto-delete and absolute bounds replace
  those safeguards. pi-gcp still owns VM/disk/firewall deletion readback.

## Tests authored, not run

Existing bindings, recipe, scheduling and process tests remain. Twenty
new methods cover the GCE procedure and receipt rejection paths:

- `test_gce_allocation_shape_and_project_refuse`
- `test_gce_backstop_outside_bounds_refuses`
- `test_gce_credentials_present_refuses`
- `test_gce_exact_termination_boundaries_pass`
- `test_gce_extra_procedure_field_refuses`
- `test_gce_late_start_cannot_hide_inside_ttl_tolerance`
- `test_gce_missing_procedure_field_refuses`
- `test_gce_old_launchd_procedure_refuses`
- `test_gce_procedure_passes_preflight`
- `test_gce_provider_ttl_false_refuses`
- `test_gce_remaining_pinned_procedure_fields_refuse`
- `test_gce_service_account_present_refuses`
- `test_gce_status_not_running_refuses`
- `test_gce_stop_action_refuses`
- `test_gce_termination_709_minutes_59_seconds_refuses`
- `test_gce_termination_719_minutes_31_seconds_refuses`
- `test_gce_wrong_boot_disk_size_refuses`
- `test_gce_wrong_boot_disk_type_refuses`
- `test_gce_wrong_mechanism_refuses`
- `test_gce_wrong_source_image_refuses`

No baseline test method was deleted. Two initial, uncommitted 720/709-minute
cases were replaced by the required 719:31 and 709:59 cases after the lead
tightened the bounds. These existing methods were updated:

- test_backstop_is_separate_from_session_and_bounds_retrieval retains the
  720-minute versus earlier-backstop distinction. GCE TTL replaces launchd
  calendar data, and it checks a provider bound earlier than client backstop.
- test_live_receipt_exact_binding_and_no_spawn_on_rejection retains exact
  identity and no-spawn rejection, now using GCE VM fields.
- test_stop_receipt_requires_actual_pi_gcp_fields_and_state retains exhaustive
  missing-field rejection and policy/type failures. Its obsolete plist,
  calendar, caffeinate and launchctl cases are replaced by GCE receipt cases.
- test_timestamp_normalization_and_full_enforcement_invocation retains generic
  ISO fraction/Z parsing and the complete argv. A fractional live allocation
  now rejects because the GCE receipt contract requires whole seconds.
- Existing live/start fixtures now pass the explicit procedure dependency.
  No other preflight input contract is changed.

## Verification

Python 3.11.15 py_compile of run.py and test_driver.py passed, exit 0 with
empty output. Python 3.10 grammar parsing passed. These are syntax checks only.
Runtime/unit tests and provider behavior are unrun locally under the Mac hold;
Linux CI is required.
The production Python diff is 81 added + 105 removed = 186 changed lines against
25b8dc6e. The control JSON diff is reported separately from executable code.

- `study/storage/pod/run.py`: `b8386bc5fe79a2f554eca8ab45a9d5c522ed6cfc3d07d4fe2e374e6a7c13e714`
- `study/storage/pod/test_driver.py`: `6c02e2f4ff511d1643424e4c3fbf6a16a2e647058097c5d59b336b5f0249ddc5`
- `study/storage/pod/control/stop-procedure.json`: `cdcd4545f3c250bc76106a306eb4681ee03d9113fef3640b5693045859112c7e`

---

# Earlier phase-1 evidence (historical)

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
