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
        self.assertEqual(len(timed), 48)
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

    def test_timing_namespaces_refuse_individual_reuse_and_adoption(self):
        from io import StringIO
        from types import SimpleNamespace
        from unittest.mock import Mock
        import sys

        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder)
            work, scratch = root / "work", root / "scratch"
            data = work / "timing-data"
            data.mkdir(parents=True)
            index = data / "int8-n10000-d384.bin"
            index.write_bytes(b"immutable-input")
            name = "n10000-d384-w1-rep1"
            selected = root / "selected.json"
            selected.write_text(json.dumps([name]))
            sample = {"timestamp": 1, "load1": 20, "load5": 18,
                "load15": 17, "level": 20, "pressure_state": 1,
                "available_bytes": 4 * 1024**3, "swapouts": 10000,
                "page_size": 16384, "swap_usage": "used = 100M",
                "ac_power": True}
            telemetry = SimpleNamespace(native_resources=Mock(
                side_effect=lambda _: dict(sample)))
            spec = SimpleNamespace(name="storage_telemetry",
                loader=SimpleNamespace(exec_module=Mock()))
            result = {"queries": 256, "n": 10000, "dimensions": 384,
                "workers": 1, "candidate_pool":
                "global quantized top128, then original F32 rerank",
                "latency_ns": {"count": 256, "p50": 100, "p95": 200}}
            outputs = []

            def execute(command, **options):
                path = Path(command[command.index("-output") + 1])
                outputs.append(path)
                self.assertEqual(command[command.index("-data-dir") + 1],
                                 str(data))
                with path.open("x") as stream:
                    json.dump(result, stream)
                return SimpleNamespace(returncode=0)

            def invoke(directory):
                argv = ["run.py", "timing", "--execute", "--jobs-file",
                        str(selected), "--output-dir", str(directory)]
                with patch.object(run.sys, "argv", argv):
                    run.main()

            with (patch.object(run, "WORK", work),
                  patch.object(run, "SCRATCH", scratch),
                  patch.object(run.sys, "platform", "darwin"),
                  patch.object(run.platform, "machine", return_value="arm64"),
                  patch.object(run, "build") as build,
                  patch.object(run, "source_fingerprint", return_value="pin"),
                  patch.object(run.subprocess, "check_output",
                               return_value="go1.27.1 darwin/arm64"),
                  patch.object(run.subprocess, "run",
                               side_effect=execute) as go,
                  patch.object(run, "compatible_prior") as adopt,
                  patch.object(run.importlib.util, "spec_from_file_location",
                               return_value=spec),
                  patch.object(run.importlib.util, "module_from_spec",
                               return_value=telemetry),
                  patch.dict(sys.modules), patch.dict(run.os.environ),
                  patch("sys.stdout", StringIO())):
                first = root / "attempt1/sample"
                second = root / "attempt2/sample"
                invoke(first)
                receipt_path = first / (name + ".json")
                receipt = json.loads(receipt_path.read_text())
                self.assertFalse(receipt["valid_timing_comparison"])
                self.assertEqual(receipt["artifact_sha256"], run.file_digest(
                    first / (name + ".result.json")))
                receipt["source_fingerprint"] = "old-source"
                receipt_path.write_text(json.dumps(receipt))
                original = receipt_path.read_bytes()
                with self.assertRaisesRegex(RuntimeError, "whole groups"):
                    invoke(first)
                adopt.assert_not_called()
                self.assertEqual(go.call_count, 1)
                invoke(second)
                self.assertEqual(outputs, [first / (name + ".result.json"),
                                           second / (name + ".result.json")])
                self.assertEqual(receipt_path.read_bytes(), original)
                self.assertEqual(index.read_bytes(), b"immutable-input")
                self.assertFalse((data / (name + ".json")).exists())
                third = root / "attempt3/sample"
                third.mkdir(parents=True)
                (third / (name + ".result.json")).write_text("partial")
                before = telemetry.native_resources.call_count
                with self.assertRaisesRegex(RuntimeError, "unrecorded output"):
                    invoke(third)
                self.assertEqual(go.call_count, 2)
                self.assertEqual(telemetry.native_resources.call_count, before)
                adopt.assert_not_called()
                for names in ([name, name], [name + "/alias"]):
                    selected.write_text(json.dumps(names))
                    before_build = build.call_count
                    unused = root / "rejected/sample"
                    with patch("sys.stderr", StringIO()):
                        with self.assertRaises(SystemExit):
                            invoke(unused)
                    self.assertEqual(build.call_count, before_build)
                    self.assertEqual(go.call_count, 2)
                    self.assertFalse(unused.exists())

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
