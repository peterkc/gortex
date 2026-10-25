# Neutral dataset format: gxstudy-v2

The format is independent of a storage engine. Adapters consume these records,
not the source SQLite store. Graph file paths are data, not instructions to read
local files. `dataset/dataset.go` defines the Go types.

## Record files

A tier contains `manifest.json`, `queries.json`, `expected.json`, and a directory
for each repository. A repository contains its manifest and six record files:

| File            | Record contents                                             |
|-----------------|-------------------------------------------------------------|
| `nodes.jsonl`   | Typed identity/location fields and the remaining row payload |
| `edges.jsonl`   | Typed endpoints/kind/location/confidence and row payload     |
| `files.jsonl`   | `path`, `content_hash`, `size`                                |
| `symbols.jsonl` | `node_id`, `tokens`                                          |
| `content.jsonl` | `chunk_id`, `node_id`, `file_path`, `body`                     |
| `vectors.bin`   | Length-prefixed ID, then 384 little-endian float32 values     |

JSONL is one UTF-8 JSON object per LF-terminated line. Strings escape embedded
newlines. Source spans use one-based, inclusive line numbers; zero means no span.
File hashes and sizes come from the source index. Adapters must retain duplicate
edges and all record fields. Dangling endpoints are valid; do not invent nodes.

A node's typed fields are `id`, `kind`, `name`, `qual_name`, `file_path`,
`start_line`, `end_line`, and `language`. Its `payload` map retains every other
SQLite node column, including generated columns and nulls. This includes doc,
signature, column positions, visibility, flags, search fields, meta and provenance.
An edge's typed fields are `from`, `to`, `kind`, `file_path`, `line`, `cross_repo`
and `confidence`. Its payload retains the rest of its SQLite row, including row
ID, origin, tier, confidence label, resolution fields and generated fields.

Payload values are JSON values. SQLite `meta` BLOBs become base64 JSON strings;
SQL NULL stays JSON null. Payload is opaque source-row data. Namespace rewriting
changes the typed IDs, not embedded source identifiers in payload. The synthetic
delta also retains inherited payload rather than pretending to re-extract code.
Point, file, name and edge answers must round-trip payload, not only identity.

A binary vector record contains a uint32 little-endian byte length, that many
UTF-8 ID bytes, then exactly 384 IEEE-754 float32 values. There is no file header,
padding or dimension field. The manifest supplies the record count. The loader
rejects a partial record, empty ID, ID longer than 1 MiB, or nonfinite value.
Vectors are normalized synthetic data, not model embeddings.

## Manifests and provenance

Every manifest records `generator_version`, `seed`, `source_revision`,
`source_sha256`, `counts` and `sha256`. Repository manifests add `repo`, `origin`
and, after qualification, `namespace`. Derived repositories record `parent_repo`
and `derived_fraction`. `cross_repo_policy` labels synthetic call rewriting.
A mixed-source tier keeps each source revision/database hash in its part manifest.

The repository checksum map covers all six record files. The tier checksum map
covers those files, repository manifests, queries and expected answers. A manifest
does not hash itself. Results record the tier-manifest digest. `dataset.Load`
checks the version, checksums, record counts, vector structure and unique,
nonempty node IDs. This is structural validation, not graph completeness proof.

`origin=real` identifies an exported graph, not real embeddings or synthetic
cross links. `origin=derived` identifies a file-subset variant. Names and paths
remain source data and can be private. Do not publish dataset contents or source
mappings merely because output directories use public labels.

## Exporting a multi-prefix source store

A source store can contain several real repository prefixes. One output part is
created per prefix. `-roots` maps exact source prefixes to `{id, tree, revision}`;
`id` is the public output label. Each node is assigned once across those parts.
Real ownership comes from its source prefix, file ownership or qualified ID.

An unowned node is assigned to the lowest-labelled real repository incident to
it, using endpoint/file ownership. If it has no such reference, it goes to the
lowest-labelled part. Unowned symbols follow their node assignment. This keeps
unowned dependencies from multiplying across a single multi-prefix export.
An edge is stored with its source node's assigned part. Its target keeps the
qualified ID of the target's assigned part. `cross_repo` is true only when both
endpoints have different real owning prefixes. Physical placement of an unowned
node never turns its edges into semantic repository crossings.

The manifest's `namespace` prevents a later real-source tier from prefixing an
already-qualified foreign endpoint with the local repository ID. Real cross
edges retain their targets. Synthetic linking skips existing cross edges.
A subset can still have a dangling external target; that is not a missing node
that an adapter may repair.

## Source content

Function/method spans are read only from the indexed source tree. Each span is
split into chunks of at most 160 source lines. There is no byte-size cap.
Symlinks must still resolve inside that tree. Unreadable or invalid spans are
skipped. `skipped_spans` counts function/method nodes with no content chunk,
not the number of omitted chunks. The baseline skips 991 span-less nodes.
The original content FTS rows are not copied.

## Deterministic derived tiers and vectors

For rank `r` starting at 1, the default file fraction is
`0.02 + 0.98 / pow(r, 1.15)`. Select `ceil(fraction * file_count)` files by SHA-256
byte order of `seed:path`. Retain selected file-owned nodes and every file-less
node. Retain file-owned edges only for selected files and remove edges to known
nodes in omitted files. Unknown endpoints remain. Fraction 1 preserves all rows.
This means file-less nodes repeat across derived variants, unlike a single
multi-prefix source export, where each node is assigned once.

IDs use `repositoryID::sourceID`. For an existing namespace, only local IDs are
rewritten; qualified foreign targets stay qualified. File paths and names do not
change. Every hundredth local call is retargeted into the next repository's
file-owned function/method nodes, ordered by seeded hash. One quarter of these
links concentrate on up to three seeded hub targets; the rest cycle through all
targets. The edge is flagged
cross-repository. Existing real cross edges are not changed.

This synthetic linking is the default (`gen -synthetic-cross-repo=true`). With
`-synthetic-cross-repo=false`, `gen` skips it and preserves the input edges.
Neither repository nor tier manifests receive a new synthetic linking policy in
that mode. Other derivation and workload rules remain unchanged.

`MakeVector` creates 32 seeded clusters. Raw unit vectors come from SHA-256 blocks
of `seed:id:block`, interpreted as signed 16-bit components. A vector mixes
0.8 times its cluster center with 0.2 times node-specific noise, normalizes in
float64, then rounds to float32. A node ID's hash selects its cluster. There is
one vector per function/method, even if its source span is absent.

There are twelve single-repository held-out vector queries, and twelve more
multi-repository queries. Their IDs are absent from the index. They use the same
cluster distribution, not stored vectors. The delta uses a separate perturbed
near-neighbour probe. These synthetic clusters do not establish semantic quality.

## Workloads and oracle answers

`queries.json` is an ordered array of `workload.Query`. `expected.json` stores
one matching `query_id` and complete `Answer` per request. Seed 20261022 produces
683, 765 and 945 requests for the current tier1, tier10 and tier100 datasets.
Counts depend on the input graph, not only the tier name.

Point/name/file keys use seeded hash selection. Edge requests use distinct IDs
from degree quartiles plus the largest hub. All edge kinds with at least 100
edges, calls, references and `any` are covered. Caller traversals use a larger
stratified call-target sample. No operation/repository/kind/key combination is
repeated. Multi-name keys favour repository coverage and return all matches for
selected names with at most 2,000 matches. Cross-edge/traversal targets span
several repository pairs rather than only the first sorted pair.

`NodeByID` returns `adapter.ErrNotFound` only for an absent node. All list methods
return empty,nil for unknown keys. Name matching is exact and case-sensitive.
Name results sort by ID before the limit; zero is empty and negative is unlimited.
File/edge order is unspecified and comparison is an order-independent multiset
comparison of full records. Edge kind `any` or empty means every kind.

Caller traversal is breadth-first over incoming `calls`. Sort/deduplicate caller
IDs at each node, keep the first 20, visit an ID once, exclude the start, and stop
after 1,000 returned IDs. Depth two or three includes every visited frontier,
not only the last one. The final IDs are sorted. Multi-repository traversal uses
fan-out incoming edges and can cross real or synthetic repository boundaries.
Optional native traversal must use these same caps and ordering semantics.

The lexical match rule tokenizes the query and document into lowercase Unicode
letter/digit runs. A document matches when it contains at least one unique query
term (OR). Empty/absent terms produce no matches. Symbols match `tokens`; content
matches `body`. The harness prepares the full trusted match set from these input
records, independently of the adapter's returned IDs or scores.

The harness-formula BM25 reference uses k1=1.2, b=0.75, unique query terms and
`idf=log(1+(N-df+0.5)/(df+0.5))`. It sums OR-match scores and breaks score ties by
ID. The global multi reference uses global document frequency/average length.
The local multi reference merges per-repository formula scores. Cosine uses
float64 accumulation and ID tie-breaking.

`RankPolicy.Mode` selects `"harness"` (the omitted default) or `"native"` for
lexical operations. Harness mode retains the 0.8 overlap floor; backend `Exact`
and `MinOverlap` may strengthen it. Default `multi_symbols` must match the exact
local formula merge. Native multi APIs have independent `native_multi_*` policy
keys and use the global reference in harness mode.

Native mode accepts engine-native lexical scores, such as FTS5 bm25 or Bleve
BM25 with coordination. Every hit must be a distinct, nonempty, matching ID, with
a finite score in descending order. The result must contain at most k hits and
cannot be empty when the oracle has hits. Single-repository hits must also belong
to that repository. Scores must use higher-is-better order; an adapter can reverse
a distance/lower-is-better score without adopting the harness formula. Formula
local/global overlap is quality only, not a native correctness threshold.
Backend `Exact`/`MinOverlap` apply only in harness mode. This mode does not change
query-specific exact delta marker coverage or required/forbidden-ID checks.

If default `multi_symbols` lacks an entry, it inherits a native `symbols` mode.
An explicit entry selects its own mode. Native API keys never inherit that mode.
In native default fan-out, the harness requests k hits per repository, validates
those actual inputs and requires the output to be their global score-descending,
ID-tied top-k, including unchanged scores. It does not gate on formula ranks.
The inputs are captured during the existing calls, then checked outside timing.
Native multi APIs use the same lexical match/shape rules, but no harness-owned
input merge exists to verify. Cosine/vector policies are unchanged.

Symbol/vector hit IDs are node IDs; content hit IDs are chunk IDs. Overlap@k
divides unique shared hits by the reference count (up to k), not always k.
Both-empty is 1; oracle-empty/adapter-nonempty is 0. `local_overlap_at_k` and
`global_overlap_at_k` report both reference qualities per check. Their operation
summaries are `Result.local_quality` and `Result.global_quality` (min/mean/p50/count).
For a single-repository request both references are scoped to that repository.
Existing `overlap_at_k` reports overlap against the selected operation reference.
Neither local nor global overlap fails a regular native-scoring request.

All policies fail invalid hits, empty answers when the oracle has hits, missing
required IDs and forbidden deleted IDs. Harness `ExactRank` keeps the same hit
count and permits only cutoff substitutions within 1e-5 relative score error,
verified by oracle-provided replacement scores. Raw overlap remains visible even
when a cutoff tie is accepted. Delta marker searches keep their query-specific
exact coverage over all expected matching documents in either scoring mode.

## File changes

`FileChange` replaces one file's nodes, file metadata, symbol/content/vector
records, and edges owned by the file or sourced by its old nodes. Incoming edges
from other files remain, even if a deleted target becomes dangling. Add the
replacement records after removing the old ownership set. Inputs are immutable.

The shared delta selects a Go file with at least two function/method nodes by
file hash. It renames one function, deletes another, adds a function, removes
owned edges to/from the deleted symbol, redirects an edge and adds a call edge.
It removes/adds vectors and text documents. It probes old/new names, removed and
added IDs, changed edges, retained dangling incoming edges, exact `deltaprobe`
search results and vector neighbours. The inserted vector must be present in the delta top-k. The content hash is a synthetic record
hash, not a hash of an edited source file. This is not a parsing benchmark.
`BuildStats.Mode` must explicitly say `full` or `delta`.

## Runner safety timeout

Runner binaries accept `-safety-timeout`, a positive Go duration with a `30s`
default. One guard covers the complete untimed commit-race pass. A separate
guard covers each crash child, including full segment verification Opens.
These are safety limits, not latency measurements or a deadline for the full run.
The concurrent reader handshake and minimum-sample timeouts remain 30 seconds.

Fresh-process request JSON stores `SafetyTimeout` as integer nanoseconds.
A missing or zero field in an older saved request retains the 30-second default;
negative values are rejected. The CLI rejects zero and negative durations before
creating an output directory or starting a child.

## Timing-only result mode

`-timing-only` defaults to false. A true value travels in fresh-process request
JSON as `TimingOnly`. A missing field in a saved request selects the unchanged
full protocol. Timing mode stops after the base and optional-native query
workloads, including their warm-ups and answer checks. Build/seal, first/warm
Open, resource samples, segment closes and server cleanup still run.

A timing-only result contains `timing_only: true` and this explicit list:

```json
"skipped_phases": ["delta", "concurrent_readers", "commit_race", "crash_trials"]
```

Its `delta`, `delta_checks`, `concurrent` and `crash` fields are absent; there is
no `delta_build` metric. `success=true` means only that this selected base
workload and cleanup succeeded. It MUST NOT stand in for a correctness pass.
Correctness consumers must reject `timing_only=true` and require the full
protocol's phase coverage. Base-workload answer checks do not prove update,
publication, reader or crash correctness.

With the option absent or false, the full protocol and its JSON fields remain
unchanged. `timing_only` and `skipped_phases` are omitted in that mode. A discarded
timing-only warm-up status also records `timing_only=true`; it is not a full
protocol result.
