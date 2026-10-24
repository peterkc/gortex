# Hybrid-vector Mac accuracy report

Local-only study evidence. Recorded 2026-10-23 06:42 PM PDT.

## Decision summary

At 100k, both M32/ef1024 methods pass mean recall 0.99 under the proposed sparse-tail rule.
Neither passes the strict no-query-below-0.8 rule. Each has one query at 0.7 in one of five builds.
On Rust10k, native F32 passes strict 0.98; I8 plus F32 rescoring passes 0.98 only under the proposal.
Beads10k and TS10k pass 0.99 under either rule. No five-build ANN setting reaches 0.999.

Int8 exhaustive quantized scan plus global128 original-F32 reranking matches the exact reference
on all 2,000 queries across the four datasets: raw recall and paper ratio are 1 for every query.
Its labelled-target hit@10 is identical to exact. This is observed accuracy, not a proof of
exactness in original cosine for arbitrary data.

The user has not chosen either mean threshold or floor rule. No production default changed.
Timing, serving RSS and the latency-budget switch point still wait for the Linux arm64 VM.

## Rules used in this report

- Mean recall: minimum of the five independent insertion-order build means.
- Strict floor: no query recall below 0.8 in any build.
- Proposed floor: in every build, at most 0.5% of queries below 0.8, none below 0.5,
  and at most one percentage point of exact-task hits lost.
- Task loss uses **gross exact hits lost / query count**, not compensation by rescued misses.
  Net task-rate loss is also in the CSV. Both interpretations give the same observed qualifications.
- "Below" is strict: a recall of exactly 0.5 is allowed by the proposed lower bound.
- With 333/334 queries, the proposal permits at most one query below 0.8 per build.
  With 1,000 queries it permits at most five. These limits apply per build, not after pooling seeds.
- An incomplete setting can fail from an observed counterexample, but cannot earn a five-build pass.

F32 uses primary K10 and a separately declared K200 task query. I8 uses K200 original-F32
rescoring for the five-build M32 feasibility comparison. Smaller K50/K100 have only first-build
evidence unless the tables explicitly show five builds. They are not silently qualified.

## Main five-build M32 / ef1024 comparison

Native F32 and I8 use M32/Add256. A/B in each decision cell means the 0.98 / 0.99 mean threshold.

| Dataset   | Storage | Worst mean | Query min | Strict A/B | Proposed A/B |
| --------- | ------- | ---------- | --------- | ---------- | ------------ |
| Beads100k | f32     | 0.993800   | 0.7       | Fail/Fail  | Pass/Pass    |
| Beads100k | i8      | 0.995500   | 0.7       | Fail/Fail  | Pass/Pass    |
| Beads10k  | f32     | 0.997305   | 0.9       | Pass/Pass  | Pass/Pass    |
| Beads10k  | i8      | 0.997006   | 0.9       | Pass/Pass  | Pass/Pass    |
| Rust10k   | f32     | 0.981982   | 0.8       | Pass/Fail  | Pass/Fail    |
| Rust10k   | i8      | 0.986787   | 0.7       | Fail/Fail  | Pass/Fail    |
| TS10k     | f32     | 0.996396   | 0.8       | Pass/Pass  | Pass/Pass    |
| TS10k     | i8      | 0.996096   | 0.9       | Pass/Pass  | Pass/Pass    |

### Per-build tails and task losses

Arrays follow build order 1–5. All eight groups have `[0,0,0,0,0]` queries below 0.5.
Task losses are exact-positive hits that ANN misses, before subtracting rescued exact misses.

| Dataset   | Storage | Below 0.8/build | Exact hits lost/build | Max gross loss |
| --------- | ------- | --------------- | --------------------- | -------------- |
| Beads100k | f32     | [1,0,0,0,0]     | [3,2,1,3,2]           | 0.300 points   |
| Beads100k | i8      | [0,0,0,1,0]     | [3,3,4,3,2]           | 0.400 points   |
| Beads10k  | f32     | [0,0,0,0,0]     | [0,0,0,1,0]           | 0.299 points   |
| Beads10k  | i8      | [0,0,0,0,0]     | [0,0,1,0,0]           | 0.299 points   |
| Rust10k   | f32     | [0,0,0,0,0]     | [1,1,0,0,2]           | 0.601 points   |
| Rust10k   | i8      | [1,0,0,0,0]     | [1,1,1,1,0]           | 0.300 points   |
| TS10k     | f32     | [0,0,0,0,0]     | [0,0,0,0,0]           | 0.000 points   |
| TS10k     | i8      | [0,0,0,0,0]     | [0,0,0,0,0]           | 0.000 points   |

At 100k, the F32 below-0.8 query is in build1; the I8 query is in build4.
At Rust10k, I8 has one such query in build1. These sparse exceptions cause the strict/proposed split.

### Distance ratio and task accuracy

The paper metric is Eq.(3): per query, `10 / sum(returned_distance_i / exact_distance_i)`,
with rank-matched original cosine distances. It is not the mean of inverse ratios.
The table reports the worst build mean and the full task-hit range.

| Dataset   | Storage | Worst ratio mean | Exact task | ANN task range     |
| --------- | ------- | ---------------- | ---------- | ------------------ |
| Beads100k | f32     | 0.999624         | 0.784000   | 0.781000–0.784000  |
| Beads100k | i8      | 0.999595         | 0.784000   | 0.780000–0.783000  |
| Beads10k  | f32     | 0.999664         | 0.880240   | 0.877246–0.880240  |
| Beads10k  | i8      | 0.999761         | 0.880240   | 0.877246–0.880240  |
| Rust10k   | f32     | 0.999241         | 0.720721   | 0.714715–0.720721  |
| Rust10k   | i8      | 0.999400         | 0.720721   | 0.717718–0.720721  |
| TS10k     | f32     | 0.999702         | 0.672673   | 0.672673–0.672673  |
| TS10k     | i8      | 0.999664         | 0.672673   | 0.672673–0.672673  |

High average distance ratio does not prove strict raw-ID recall. The raw-ID and task metrics
answer different questions. The task has one labelled symbol and does not credit other relevant
symbols. Exact is not a human relevance oracle.

## Int8 scan baseline

| Dataset   | Records | Queries | Mean/min recall | Mean/min ratio | Task hit@10 |
| --------- | ------- | ------- | --------------- | -------------- | ----------- |
| Beads100k |  101994 |    1000 | 1.0 / 1.0       | 1.0 / 1.0      | 0.784000    |
| Beads10k  |   10232 |     334 | 1.0 / 1.0       | 1.0 / 1.0      | 0.880240    |
| Rust10k   |   10507 |     333 | 1.0 / 1.0       | 1.0 / 1.0      | 0.720721    |
| TS10k     |   10353 |     333 | 1.0 / 1.0       | 1.0 / 1.0      | 0.672673    |

All four baseline runs have zero exact-task hits lost and zero rescued misses.
These are deterministic scan indexes, not five random ANN builds.
The 100k set uses 1,000 queries, while the old beads set uses its nested 334-query sample.
Do not attribute the different exact task averages solely to corpus size without a paired analysis.

## Stored-vector sizes

Bytes below use build1 of M32. I8 includes the original F32 sidecar required for actual rescoring.
The scan file already contains both quantized values and original F32. Benchmark ID/parent JSON
is listed separately and is not included in vector bytes. SQLite payload/catalog costs are excluded.

| Dataset   | Scan incl. F32 | Native F32 | I8 + required F32 | Shared ID JSON |
| --------- | -------------- | ---------- | ----------------- | -------------- |
| Beads10k  |     53,323,036 | 44,737,576 |        55,215,144 |      2,409,233 |
| Rust10k   |     54,748,323 | 45,939,832 |        56,699,000 |      2,548,960 |
| TS10k     |     54,214,570 | 45,266,500 |        55,867,972 |      3,032,286 |
| Beads100k |    531,536,563 | 445,946,824 |       550,388,680 |     24,131,324 |

At 100k, the I8 graph is 132,621,256 bytes, but its required F32 sidecar adds 417,767,424.
The complete 550,388,680-byte vector method is about 23% larger than native F32 here.
It is not a fourfold complete-storage saving. Required vector bytes for every measured build
are in `accuracy-per-build.csv`. Accuracy RSS includes benchmark oracles and is not serving RSS.

## Coverage, skips and limits

- Completed **180 jobs**, including **126 query-result files** across **51 distinct settings**.
  All completed Go jobs exited 0. No accuracy process remains active.
- The original matrix had 358 jobs. The later M16 Rust/TS F32 qualification requirement added
  24 possible jobs, for a current plan of 382. Exactly 202 current-plan jobs were not run.
  Every exact skipped command and reason is in `accuracy-skips.json`.
- M16 100k F32/I8 and Rust10k I8 have strict-floor failures. The lead stopped further M16
  qualification because the large-tier switch cannot use those strict-floor results as a default.
- **Proposed-rule limit:** each single 100k M16 probe passes observed 0.98 plus proposed tails
  (F32: three below 0.8; I8: five; none below 0.5; seven task hits lost). Neither has five builds.
  M16 therefore is not proven ineligible under the proposal. It was not resumed after the stop.
- The lead stop arrived after an awaited M16 batch completed. Those completed small-set results
  remain measured evidence, not falsely labelled skips. No further M16 computation followed.
- M32/ef512 rows pruned under the earlier strict floor remain incomplete under the proposal
  where the observed sparse tail now passes. The final stop did not authorise restarting them.
- Remaining I8 K50/K100 seeds were skipped after the first-build comparison. K200 is the
  five-build feasibility case, not proof that smaller K behaves identically in every seed.
- Sampling, model, input text and query eligibility match the embed-quality builder. Chunk
  expansion makes 100,000 symbols produce 101,994 vectors. This exceeds the Gortex default
  100,000-text refusal point; the study cap is explicitly 110,000.
- The API step remains 2,030,395 new tokens, 469 calls, no pending reservation, within its 6M cap.
  No new API calls were made during accuracy execution.
- Mac load and memory pressure came from non-study work. One pre-job memory window failed
  with 30,181 Swapouts. It stopped the original controller before the next job. After clearance,
  the revised controller allowed up to six five-minute waits, with unique failed-window logs.
  No failed window occurred after that change, so the real retry path was not exercised.
  Focused tests cover recovery, six-wait exhaustion and non-memory errors.
- All Mac elapsed times are unusable. No `timing` or `latency` execution occurred. The Ubuntu
  Linux arm64 native link, saved-index cross-open, clean latency, serving RSS and switch point
  remain unverified. Pure-Go timing only has a compile check, not a Linux runtime result.

## Complete setting appendix

Notation: setting is storage/M/ef/K. `n` is measured builds. Mean is the worst observed mean;
it is a worst-of-five result only when n=5. Arrays follow the measured seed order.
Every row has zero queries below 0.5 in every measured build. The explicit per-build zero
counts, all per-build means/minima, task losses/rescues, both loss definitions and storage sizes
are in `accuracy-per-build.csv`; all 126 original result files retain per-query values.
`accuracy-summary.json` gives strict/proposed verdicts at 0.98/0.99/0.999 for all 51 settings.
It distinguishes observed failure from incomplete qualification.

### Beads10k

| Setting            | n | Worst mean | Min | Below .8/build | Exact hits lost/build |
| ------------------ | - | ---------- | --- | -------------- | --------------------- |
| f32/16/1024/10     | 1 | 0.993713   | 0.7 | [1]            | [0]                   |
| f32/16/512/10      | 1 | 0.988623   | 0.6 | [2]            | [3]                   |
| f32/32/1024/10     | 5 | 0.997305   | 0.9 | [0,0,0,0,0]    | [0,0,0,1,0]           |
| f32/32/512/10      | 1 | 0.994012   | 0.7 | [2]            | [1]                   |
| i8/16/1024/100     | 5 | 0.989222   | 0.5 | [0,0,3,1,0]    | [1,1,5,1,1]           |
| i8/16/1024/200     | 5 | 0.989222   | 0.5 | [0,0,3,1,0]    | [1,1,5,1,1]           |
| i8/16/1024/50      | 5 | 0.989222   | 0.5 | [0,0,3,1,0]    | [1,1,5,1,1]           |
| i8/16/512/100      | 5 | 0.979641   | 0.5 | [1,1,3,3,2]    | [5,2,8,3,4]           |
| i8/16/512/200      | 5 | 0.979641   | 0.5 | [1,1,3,3,2]    | [5,2,8,3,4]           |
| i8/16/512/50       | 5 | 0.979641   | 0.5 | [1,1,3,3,2]    | [5,2,8,3,4]           |
| i8/32/1024/100     | 1 | 0.998204   | 0.9 | [0]            | [0]                   |
| i8/32/1024/200     | 5 | 0.997006   | 0.9 | [0,0,0,0,0]    | [0,0,1,0,0]           |
| i8/32/1024/50      | 1 | 0.998204   | 0.9 | [0]            | [0]                   |
| i8/32/512/100      | 1 | 0.995210   | 0.8 | [0]            | [2]                   |
| i8/32/512/200      | 2 | 0.995210   | 0.7 | [0,1]          | [2,0]                 |
| i8/32/512/50       | 1 | 0.995210   | 0.8 | [0]            | [2]                   |
| scan/K128          | 1 | 1.000000   | 1.0 | [0]            | [0]                   |

### Rust10k

| Setting            | n | Worst mean | Min | Below .8/build | Exact hits lost/build |
| ------------------ | - | ---------- | --- | -------------- | --------------------- |
| f32/16/1024/10     | 5 | 0.978679   | 0.8 | [0,0,0,0,0]    | [2,1,2,2,3]           |
| f32/32/1024/10     | 5 | 0.981982   | 0.8 | [0,0,0,0,0]    | [1,1,0,0,2]           |
| f32/32/512/10      | 1 | 0.977778   | 0.7 | [4]            | [1]                   |
| i8/16/1024/200     | 1 | 0.984084   | 0.7 | [2]            | [3]                   |
| i8/32/1024/100     | 1 | 0.986787   | 0.7 | [1]            | [1]                   |
| i8/32/1024/200     | 5 | 0.986787   | 0.7 | [1,0,0,0,0]    | [1,1,1,1,0]           |
| i8/32/1024/50      | 1 | 0.986787   | 0.7 | [1]            | [1]                   |
| i8/32/512/100      | 1 | 0.976577   | 0.6 | [4]            | [3]                   |
| i8/32/512/200      | 1 | 0.976577   | 0.6 | [4]            | [3]                   |
| i8/32/512/50       | 1 | 0.976577   | 0.6 | [4]            | [3]                   |
| scan/K128          | 1 | 1.000000   | 1.0 | [0]            | [0]                   |

### TS10k

| Setting            | n | Worst mean | Min | Below .8/build | Exact hits lost/build |
| ------------------ | - | ---------- | --- | -------------- | --------------------- |
| f32/16/1024/10     | 5 | 0.991291   | 0.7 | [0,1,0,0,0]    | [0,0,0,0,0]           |
| f32/32/1024/10     | 5 | 0.996396   | 0.8 | [0,0,0,0,0]    | [0,0,0,0,0]           |
| f32/32/512/10      | 3 | 0.991592   | 0.6 | [0,0,1]        | [0,0,0]               |
| i8/16/1024/200     | 5 | 0.988589   | 0.8 | [0,0,0,0,0]    | [0,0,0,0,0]           |
| i8/16/512/200      | 1 | 0.977477   | 0.6 | [4]            | [0]                   |
| i8/32/1024/100     | 1 | 0.997297   | 0.9 | [0]            | [0]                   |
| i8/32/1024/200     | 5 | 0.996096   | 0.9 | [0,0,0,0,0]    | [0,0,0,0,0]           |
| i8/32/1024/50      | 1 | 0.997297   | 0.9 | [0]            | [1]                   |
| i8/32/512/100      | 1 | 0.993093   | 0.8 | [0]            | [0]                   |
| i8/32/512/200      | 5 | 0.993093   | 0.8 | [0,0,0,0,0]    | [0,0,0,0,0]           |
| i8/32/512/50       | 1 | 0.993093   | 0.8 | [0]            | [1]                   |
| scan/K128          | 1 | 1.000000   | 1.0 | [0]            | [0]                   |

### Beads100k

| Setting            | n | Worst mean | Min | Below .8/build | Exact hits lost/build |
| ------------------ | - | ---------- | --- | -------------- | --------------------- |
| f32/16/1024/10     | 1 | 0.986800   | 0.5 | [3]            | [7]                   |
| f32/32/1024/10     | 5 | 0.993800   | 0.7 | [1,0,0,0,0]    | [3,2,1,3,2]           |
| f32/32/512/10      | 1 | 0.989800   | 0.6 | [2]            | [5]                   |
| i8/16/1024/200     | 1 | 0.987000   | 0.5 | [5]            | [7]                   |
| i8/32/1024/100     | 1 | 0.995500   | 0.8 | [0]            | [3]                   |
| i8/32/1024/200     | 5 | 0.995500   | 0.7 | [0,0,0,1,0]    | [3,3,4,3,2]           |
| i8/32/1024/50      | 1 | 0.995500   | 0.8 | [0]            | [3]                   |
| i8/32/512/100      | 1 | 0.991300   | 0.7 | [1]            | [5]                   |
| i8/32/512/200      | 1 | 0.991300   | 0.7 | [1]            | [5]                   |
| i8/32/512/50       | 1 | 0.991300   | 0.7 | [1]            | [5]                   |
| scan/K128          | 1 | 1.000000   | 1.0 | [0]            | [0]                   |

## Integrity and checks

Go runtime sources, vector inputs, native library and toolchain stayed unchanged throughout
accuracy execution. Only controller memory/scheduling policy changed. Archived controllers
and `orchestration-adoption.json` preserve the compatibility proof instead of rewriting receipts.
All original accepted study manifests remain unchanged. Current evidence fingerprints cover
this report and derived summaries; historical preparation and partial reports stay available.

Eleven focused Python tests pass. Go tests/vet pass for all five packages. The new retry tests
use synthetic process boundaries and do not wait or touch native indexes. No race check ran.
This is a benchmark report, not a production integration or a hostile-native-file validator.
The accepted report's malformed-file warning still applies.
