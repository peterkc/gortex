# Catalog and serving adapter recovery

All 37 implementation and test files are exact restorations. Their SHA-256
values match the ten c72bcd5d host-rebuild receipts saved by the harness worker.
No implementation was rewritten from memory or a summary.

Seven current module manifests match their historical hashes. Six were restored
from fully printed text. The catalog-pebble manifest was reconstructed and then
reproduced exactly by native metadata resolution. The three Bleve manifests
remain derived from recorded pins. All eight go.sum files are derived and differ
from the original checksum files.

## Scope and method

Recovery used the explicitly authorized deleted-worker transcripts:

- `sub-fcaf92f9`: sqlite-segment and libsql-segment.
- `sub-094d6649`: pebble-bleve, bbolt-bleve and badger-bleve.
- `sub-c20147d4`: the five catalog adapters.

These are source identifiers in the lead's local session-artifact directory,
not paths required to build the adapters. Literal source and explicit patches
were reconstructed in a private in-memory filesystem. Historical shell commands,
network calls, processes, generation and messages were not replayed. Current
`gofmt` restored source formatting before the saved file hashes were checked.

Each adapter is a separate Go 1.27.1 module. Each uses the unchanged relative
`replace gxstudy => ../../harness`. The harness implementation was not changed.
No dependency version was intentionally upgraded. The historical hashes below
preserve the remaining metadata-recovery targets, including unavailable checksum
bodies. A matching Go-file hash establishes exact restoration; a differing
metadata hash does not establish binary reproducibility.

## Per-adapter provenance and complete file list

### catalog-refs

Files: `catalog_test.go`, `files.go`, `go.mod`, `main.go`, `paths_test.go`, `store.go`.

All 5 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

No `go.sum` is needed; the module uses only the harness and standard library.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
962286de54ca06c4454014928dd66fa68c50c4beffb1fd7436d2abaa1a67993d  catalog_test.go
7507e2b72ca331df55bff6428388c9e6805c31de0abcbf13465153133f30cb69  files.go
4c47d47beecc712d40f9f3f97da46040e466455d33685b484081eba5cb4e5b74  go.mod
755c57af4c9013a95e6ec37363dce690e113f7f5ecf599d7636b74bcf8c062e7  main.go
c8f9a60dd66f44289b4abda38aaafa61d0d87502f13ba7dd36fa2552251f6ca6  paths_test.go
6296cf6a40b426e86cc5316be05f9ee3f4b33fff72e59e8f21da8cf179e27018  store.go
```

### catalog-manifest

Files: `catalog_test.go`, `files.go`, `go.mod`, `main.go`, `store.go`.

All 4 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

No `go.sum` is needed; the module uses only the harness and standard library.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
962286de54ca06c4454014928dd66fa68c50c4beffb1fd7436d2abaa1a67993d  catalog_test.go
4a99127e07731ddb5b8d9ac04b3da5039e59207ca386bab40a1ae60ca0d8242b  files.go
3c6572239571ff69f43d5c4ba2cb645474aa04707766a7cdf31fba7454a6f393  go.mod
68bc19be480531c2ed8d46f49d1e6225caf5361af2d339db58669e3e2fe35fd4  main.go
7944680c1521352d04c139463783a66589e78cc45334d5f383e9ec3394fcc7dd  store.go
```

### catalog-bbolt

Files: `catalog_test.go`, `go.mod`, `go.sum`, `main.go`, `store.go`.

All 3 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
962286de54ca06c4454014928dd66fa68c50c4beffb1fd7436d2abaa1a67993d  catalog_test.go
60b292d9378ed91316a6910b8e2791a7856df18c3eac16ac750d9f7c637bc476  go.mod
2d8c9009ecf53db52b722c99a28089a069e6e04e4e2e58b45ceec18de6e060e4  go.sum
a03493450f64b142ee33dfaba5cd45325def07a3ec34942247f888cb959dba01  main.go
ffaa24f775cefca7eef6ab58eee2d075a6f6ae37cb070dd2ca0f08d6118e8790  store.go
```

### catalog-sqlite

Files: `catalog_test.go`, `driver_test.go`, `go.mod`, `go.sum`, `main.go`, `store.go`.

All 4 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
962286de54ca06c4454014928dd66fa68c50c4beffb1fd7436d2abaa1a67993d  catalog_test.go
1d384bf016a77f9beac715ce7ad00bbc8d6d8f4af3fe82c6a6dd7f0fd9519d84  driver_test.go
e89eb67a9b5af01870248fbc3099a5da218b6a4a5d9d249d277e64b1f9d50529  go.mod
3ca6de191cbcf41056b3b2c7abdcbe4b55c7ececa358c900df16bad0a700ee95  go.sum
e5a9f72eeeb267380efb6641a802e446b839f1cabe234bdd9024b318ceb95b01  main.go
f4a045d8e95390062b40e79d588c4f8aa80625c40151ea0f596405fce92f6220  store.go
```

### catalog-pebble

Files: `catalog_test.go`, `go.mod`, `go.sum`, `main.go`, `store.go`.

All 3 Go files are exact restorations. `go.mod` was reconstructed from recorded pins. Native metadata resolution reproduced its historical SHA-256 exactly.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
962286de54ca06c4454014928dd66fa68c50c4beffb1fd7436d2abaa1a67993d  catalog_test.go
d6018bddaa91f736dedfe6a996d5f1a9459539217b8d659b4bc5d61e95c4ba5f  go.mod
496149c38514c70543003ece8d901374efdbdd038e687ac7d50a8d98c8f90173  go.sum
5a9c077596ed774dd7b288071adc97c99fae39e7844f1801702b94ee58e6f235  main.go
d1be6feb1baa37cadec466a74fab76205ee1f26096d237d6348cffb38c2a0942  store.go
```

### sqlite-segment

Files: `backend.go`, `backend_test.go`, `driver.go`, `go.mod`, `go.sum`.

All 3 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
b18e181195523f59cc89f5eed4f61db4d99e0d3d71d8596455a133a82a594395  backend.go
0bcb7bb3d623787dfc6598597d51b285d2b9f10e7a768815d04a917554adf35a  backend_test.go
1d250d56537f34df2842f9a1ad89cc933efdf7d47c1cc54e6fdb520a4cb9d0a3  driver.go
52dafa150f2daa770c6039cb08095ae26616736a57c0faa1b853db4ffbd18aa4  go.mod
3ca6de191cbcf41056b3b2c7abdcbe4b55c7ececa358c900df16bad0a700ee95  go.sum
```

### libsql-segment

Files: `backend.go`, `backend_test.go`, `driver.go`, `go.mod`, `go.sum`, `readonly_test.go`.

All 4 Go files are exact restorations. `go.mod` was restored from printed source text and still matches its historical SHA-256.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
b18e181195523f59cc89f5eed4f61db4d99e0d3d71d8596455a133a82a594395  backend.go
0bcb7bb3d623787dfc6598597d51b285d2b9f10e7a768815d04a917554adf35a  backend_test.go
36506a89878b1e01ac2d7070d4f2ea675c317618511255e965229962ea61a733  driver.go
7df7ff2108e30c9011a3def7998237787f44d552945c1a827d3b343269b10863  go.mod
7a0b5d3902cf1f36095f7b363c18f8cdf1f5b1834d0ae76ca4a7fee06e819336  go.sum
5d4e831e624d4c1173b68eba92585e7384523129f84bdd2eeb4eedae1ea15d2b  readonly_test.go
```

### pebble-bleve

Files: `cache_test.go`, `go.mod`, `go.sum`, `kv.go`, `main.go`, `main_test.go`.

All 4 Go files are exact restorations. `go.mod` was reconstructed from recorded pins. Its bytes differ from the historical file because the full indirect-requirement text was not printed. All recorded version pins were verified against the selected modules.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
c41c0e7e1ea9459ab09a0839168a28fe813c7b1bf88bb3398995b4110f3123ef  cache_test.go
54f507545c62d04e6ba80d4f2fc9a63d261350b825831968376489386e16d869  go.mod
592716e873478364c3c4362df38854341023327c692405f80201a0e041611814  go.sum
418dd54a3d2cee6097124c486706392dc59d2fb78344b80df861f29f07f57ef3  kv.go
aefd1ceb51a6ab9f5cdcaa49c5f89900bc019caa57e9fe0e8fc5840c355fb5cc  main.go
184427a486145aa232e4e8deca6c56f78e891ba9db9290a477092e6fec2fb552  main_test.go
```

### bbolt-bleve

Files: `go.mod`, `go.sum`, `kv.go`, `main.go`, `main_test.go`.

All 3 Go files are exact restorations. `go.mod` was reconstructed from recorded pins. Its bytes differ from the historical file because the full indirect-requirement text was not printed. All recorded version pins were verified against the selected modules.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
e9b872b6742f82e0b19204167a83e9384f16766e22ea688b513309088c2d9222  go.mod
11842da87cb5ab0db423d15571f812449a2261c923f4dc5ebec97c88502c0f8d  go.sum
51b386740cac42bb432c821bc8b2a5d6b314012b98064812eae082da2f7ce054  kv.go
aefd1ceb51a6ab9f5cdcaa49c5f89900bc019caa57e9fe0e8fc5840c355fb5cc  main.go
184427a486145aa232e4e8deca6c56f78e891ba9db9290a477092e6fec2fb552  main_test.go
```

### badger-bleve

Files: `cache_test.go`, `go.mod`, `go.sum`, `kv.go`, `main.go`, `main_test.go`.

All 4 Go files are exact restorations. `go.mod` was reconstructed from recorded pins. Its bytes differ from the historical file because the full indirect-requirement text was not printed. All recorded version pins were verified against the selected modules.

`go.sum` is derived from the pinned modules during verification. Its historical body was unavailable, and its bytes differ from the original checksum file.

Historical file hashes from the c72bcd5d rebuild receipt:

```text
5d6a56911b6b95511d5dbff75ede093c9a552f4b20ab52e9e733cf010ab222dc  cache_test.go
3c6823f2500889829e9280b578163fba77a3f62a07f428b1f2e2b4807083e52f  go.mod
7c04481d9f7e95fb2e7b5148d910ca7479863347bbb24cc0f697a504dcd1156b  go.sum
7ecb76cbc16f1d6b2c07771d3578f38331e04260d01a5a691ca4125c598da1b0  kv.go
aefd1ceb51a6ab9f5cdcaa49c5f89900bc019caa57e9fe0e8fc5840c355fb5cc  main.go
184427a486145aa232e4e8deca6c56f78e891ba9db9290a477092e6fec2fb552  main_test.go
```

## Verification

All ten modules passed `go vet -p=1 ./...` and `go test -p=1 ./...`, in sequence,
with `GOMAXPROCS=2`, `GOWORK=off`, `GOTOOLCHAIN=local` and `GOFLAGS=-mod=mod`.
The host was darwin/arm64, Go 1.27.1, with `CGO_ENABLED=1`. The lead granted the
single Go-process slot. It was released after checks and pin verification.

Memory level was checked before every Go command. It remained 87 at those checks,
above the required minimum of 30. The two numbers below are the levels before
successful vet/test commands.

| Adapter            | Vet exit | Test exit | Memory  |
| ------------------ | -------- | --------- | ------- |
| catalog-refs       | 0        | 0         | 87/87    |
| catalog-manifest   | 0        | 0         | 87/87    |
| catalog-bbolt      | 0        | 0         | 87/87    |
| catalog-sqlite     | 0        | 0         | 87/87    |
| catalog-pebble     | 0        | 0         | 87/87    |
| sqlite-segment     | 0        | 0         | 87/87    |
| libsql-segment     | 0        | 0         | 87/87    |
| pebble-bleve       | 0        | 0         | 87/87    |
| bbolt-bleve        | 0        | 0         | 87/87    |
| badger-bleve       | 0        | 0         | 87/87    |

The local module cache was tried first with `GOPROXY=off`. The following modules
passed entirely offline: catalog-refs, catalog-manifest, catalog-bbolt,
catalog-sqlite and sqlite-segment. Offline vet for the other five reported
specific missing pinned Pebble, Bleve, Badger or libSQL modules. Only then was
`https://proxy.golang.org` enabled for their vet/metadata resolution. All tests
and final pin queries ran offline.

`go list -p=1 -m -json` verified every requirement present in the recovered
manifests against the selected module versions. No recorded pin changed. Every
`gxstudy` replacement resolved to the sibling harness module. All 37 Go-file
hashes were checked again after verification and remain exact.

Build and test temporary files use the git-ignored scratch root
`~/.prime/research/gortex/storage-architecture/tmp/`. The existing local Go module
cache was reused. Source modules contain no cache or result folders. The catalog package tests
created six verification JSON files beside their source. Those files were moved
after testing to the scratch root's `adapter-recovery/verification/<adapter>/`
directory, without changing their bytes. The exact restored test source remains
unchanged, so future package tests can recreate those local verification files.

## Limits

- The original complete indirect-requirement text for the three Bleve modules
  was not available. Their known pins are verified, but their complete historical
  module-file bytes are not reproduced.
- None of the eight original go.sum bodies was printed. Their regenerated
  replacements are marked derived above. No byte-reproducible binary is claimed.
- No adapter binary, study dataset, corpus measurement or historical result was
  restored. Old reports, platform binaries and raw logs were not recreated.
- Package tests used their synthetic fixtures and built-in fresh-process checks.
  No study corpus was regenerated or benchmarked.
- No corpus adapter protocol, race test, cross-platform check, commit or push ran.
- This record matches the existing shared `**/RECOVERY*.md` Git exclude rule.
  Include it explicitly when publishing; the shared rule was not changed.
