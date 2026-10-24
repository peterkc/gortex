"""Plan a Go command. Execute only after the lead grants the Go slot."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

SOURCE = Path(__file__).resolve().parents[1]
SCRATCH = Path.home() / ".prime/research/gortex/storage-architecture/tmp"


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("target", choices=("ann", "timing", "sqlite-vecflat",
                                            "sqlite-usearch"))
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    if args.target in ("ann", "timing"):
        cwd = SOURCE / "hybrid-followup"
        package = "./cmd/" + args.target
    else:
        cwd = SOURCE / "adapters" / args.target
        package = "."
    binary = SCRATCH / "bin" / args.target
    environment = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off",
                       GOMAXPROCS="2")
    for key, suffix in (("GOCACHE", "cache/go-build"),
                        ("GOMODCACHE", "cache/go-mod"),
                        ("TMPDIR", "cache/tmp")):
        environment[key] = str(SCRATCH / suffix)
    native = SCRATCH / "native/usearch"
    if args.target in ("ann", "sqlite-usearch"):
        environment["CGO_ENABLED"] = "1"
        environment["CGO_CFLAGS"] = "-I" + str(native)
        runtime = "-lstdc++ -lm -pthread"
        if sys.platform == "darwin":
            runtime = ("-lc++ -framework CoreFoundation "
                       "-framework Security")
        environment["CGO_LDFLAGS"] = (
            "-L" + str(native) + " -Wl,-rpath," + str(native) + " " + runtime)
    else:
        environment["CGO_ENABLED"] = "0"
    command = ["go", "build", "-p=1", "-o", str(binary), package]
    print(json.dumps({"execute": args.execute, "cwd": str(cwd),
                      "command": command}, indent=2))
    if not args.execute:
        return
    for key in ("GOCACHE", "GOMODCACHE", "TMPDIR"):
        Path(environment[key]).mkdir(parents=True, exist_ok=True)
    binary.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(command, cwd=cwd, env=environment, check=True)


if __name__ == "__main__":
    main()
