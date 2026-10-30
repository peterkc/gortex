"""Stdlib-only controller checks. No model, build or timing run."""
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
# pod/run.py, the controller's telemetry module, imports urllib.request,
# which loads the macOS-only _scproxy when sys.platform is "darwin". Load it
# under the real platform before tests fake sys.platform.
import urllib.request  # noqa: F401

import run


class ControllerTests(unittest.TestCase):
    def test_real_plan_and_required_generated_recipes(self):
        self.assertEqual(len(run.latency_jobs(Path("ann"))), 240)
        jobs = run.latency_jobs(Path("ann"), generated=True)
        self.assertEqual(sum(j.kind == "preparation" for j in jobs), 6)
        timed = [j for j in jobs if j.kind == "timing"]
        self.assertEqual(len(timed), 30)
        for job in timed:
            flags = dict(zip(job.command[1::2], job.command[2::2]))
            self.assertIn(flags["-method"], ("exact", "int8", "f32"))
            if flags["-method"] == "f32":
                self.assertEqual((flags["-m"], flags["-ef"]), ("32", "1024"))
        self.assertEqual(len(run.timing_jobs(Path("timing"))), 60)

    def test_platform_admission_without_building(self):
        class Admitted(Exception):
            pass
        for platform, machine, admitted in (
            ("linux", "x86_64", False), ("linux", "amd64", False),
            ("linux", "arm64", False), ("linux", "aarch64", False),
            ("darwin", "arm64", True), ("darwin", "x86_64", False),
        ):
            with self.subTest(platform=platform, machine=machine):
                with tempfile.TemporaryDirectory() as folder:
                    native = Path(folder) / "native"
                    native.mkdir()
                    (native / "usearch.h").write_bytes(b"fixture")
                    (native / "libusearch_c.dylib").write_bytes(b"fixture")
                    with patch.object(run, "WORK", Path(folder)), \
                            patch.object(run.sys, "platform", platform), \
                            patch.object(run.platform, "machine",
                                         return_value=machine), \
                            patch.object(run.sys, "argv", ["run.py", "latency",
                                "--generated", "--execute", "--native-dir",
                                str(native)]), \
                            patch.object(run, "build", side_effect=Admitted) as build:
                        error = Admitted if admitted else SystemExit
                        with self.assertRaises(error):
                            run.main()
                        self.assertEqual(build.called, admitted)

    def test_generated_hash_drift(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            names = ("document.f32", "query.f32", "records.json", "query-records.json")
            for name in names:
                (root / name).write_bytes(name.encode())
            (root / "input-receipt.json").write_text(json.dumps({
                name: {"sha256": hashlib.sha256(name.encode()).hexdigest()}
                for name in names
            }))
            job = run.Job("fixture", ("ann", "-dataset", str(root), "-phase", "latency"), None, "timing")
            self.assertEqual(run.generated_input_sha(job), run.file_digest(root / "input-receipt.json"))
            (root / "document.f32").write_bytes(b"wrong")
            with self.assertRaisesRegex(RuntimeError, "input hash drift"):
                run.generated_input_sha(job)


if __name__ == "__main__":
    unittest.main()
