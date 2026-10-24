# Storage harness recovery

The accepted harness source was recovered without rewriting any implementation
from memory or summaries. Before the documentation migration below, the original
fingerprint command produced:

```text
c72bcd5d0f7f5dd9536df7cfbe38ef0a4a40881a8db5af3ba004f586565d6020  -
```

This is the accepted native-ranking revision plus the runner's `MkdirAll` fix.
It verifies the original 26 Go files and two Markdown files, using the original
`harness/` relative pathnames and native `sort` order. The independent `go.mod`
also matches its saved historical SHA-256. Its module is `gxstudy`, its Go floor
is 1.27.1, and it has no external Go dependencies.

The historical command ran from the directory containing `harness/`:

```sh
find harness -type f -name '*.go' -o -name '*.md' | sort | xargs shasum -a 256 | shasum -a 256
```

The current documentation changes that fingerprint. It must not be presented as
an unchanged current-tree fingerprint.

## Source and method

Recovery used this worker's saved source-text snapshots and its own transcript.
Literal source bodies and explicit string patches were reconstructed in a
private in-memory filesystem. Historical shell commands, generation, adapter
runs and messages were not replayed. Canonical `gofmt` restored the formatting
of source literals. The native fingerprint check then matched the accepted
revision exactly.

The runner snapshot was read after the lead's accepted parent-directory fix.
It still creates `<run dir>/segments` before invoking adapter `Build` calls.
Native lexical ranking, trusted match membership, own-score fan-out checks,
local/global quality, and their tests are unchanged.

## File provenance

These 28 files are exact restorations and remain byte-for-byte unchanged:

- `FORMAT.md`
- `adapter/adapter.go`
- `adapter/adapter_test.go`
- `cmd/export/main.go`
- `cmd/export/main_test.go`
- `cmd/gen/main.go`
- `cmd/oracle/main.go`
- `conformance/conformance.go`
- `dataset/dataset.go`
- `dataset/dataset_test.go`
- `dataset/generate.go`
- `go.mod`
- `oracle/catalog.go`
- `oracle/multi.go`
- `oracle/oracle.go`
- `oracle/oracle_test.go`
- `oracle/references.go`
- `runner/concurrent.go`
- `runner/crash.go`
- `runner/delta.go`
- `runner/native_rank.go`
- `runner/native_rank_test.go`
- `runner/publication.go`
- `runner/recheck_test.go`
- `runner/runner.go`
- `runner/runner_test.go`
- `workload/workload.go`
- `workload/workload_test.go`

`README.md` was also recovered exactly, then changed intentionally:

- Separate the source module from disposable artifacts.
- Move all example data, binary, cache and temporary paths to
  `~/.prime/research/gortex/storage-architecture/tmp/`.
- Keep explicit required CLI paths instead of adding a new implicit default.
- Update the adapter-module example for `study/storage/adapters/<name>/`.
- State the current serial verification limits and the no-race restriction.

`RECOVERY.md` is new. No file was rewritten from memory or a summary.

## Isolation and verification

The harness is a nested independent Go module. The Gortex root module and its
files were not changed. No root `go.work` exists. Verification uses `GOWORK=off`,
`GOTOOLCHAIN=local`, `GOMAXPROCS=2`, and the installed Go 1.27.1 toolchain.
Disposable verification caches and temporary files use the git-ignored durable
scratch root. No cache or result belongs in the source module.

Both required commands passed in sequence from this module with Go 1.27.1.
`GOMAXPROCS=2`, `GOWORK=off`, `GOTOOLCHAIN=local`, and `GOFLAGS=-mod=readonly`
were set. The lead granted the single Go-process slot; it was released when
the commands finished.

| Command              | Memory level before | Exit |
| -------------------- | ------------------- | ---- |
| `go vet -p=1 ./...`   | 86                  | 0    |
| `go test -p=1 ./...`  | 86                  | 0    |

Vet produced no diagnostics. Test output:

```text
ok  	gxstudy/adapter	0.143s
ok  	gxstudy/cmd/export	0.276s
?   	gxstudy/cmd/gen	[no test files]
?   	gxstudy/cmd/oracle	[no test files]
?   	gxstudy/conformance	[no test files]
ok  	gxstudy/dataset	0.178s
ok  	gxstudy/oracle	0.301s
ok  	gxstudy/runner	2.480s
ok  	gxstudy/workload	0.149s
```

All 26 Go files and `go.mod` remain unchanged after verification. The root
Gortex `go.mod` also remains unchanged. Root Gortex tests were not run.

## Limits

- No datasets, exported corpora, binaries or historical result files were recovered.
- No tier generation, candidate adapter run, corpus protocol or race test ran.
- No commit or push was made.
- Gortex localization and workspace memory were unavailable because its daemon
  was stopped. It was not started, tracked, indexed or administered.
- Required package checks do not replace future corpus or adapter validation.
