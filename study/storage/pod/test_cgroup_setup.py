"""Small cgroup fixtures; --integration requires root on Linux cgroup v2."""
import argparse
import errno
import json
import os
import select
import shlex
import subprocess
import sys
import tempfile
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

import cgroup_setup


class ArgumentTests(unittest.TestCase):
    def test_private_name_rejects_aliases_nested_names_and_shell_text(self):
        for name in ("", ".", "..", "/owned", "owned/child", "owned/..",
                     "owned\\child", "owned name", "owned;echo", "éowned"):
            with self.subTest(name=name):
                with self.assertRaises(argparse.ArgumentTypeError):
                    cgroup_setup.private_name(name)
        self.assertEqual(cgroup_setup.private_name("storage-accuracy-123"),
                         "storage-accuracy-123")

    def test_positive_limit_rejects_zero_negative_and_nonintegers(self):
        for value in ("0", "-1", "max", "1.5"):
            with self.subTest(value=value):
                with self.assertRaises(argparse.ArgumentTypeError):
                    cgroup_setup.positive(value)
        self.assertEqual(cgroup_setup.positive("64"), 64)

    def test_true_root_without_type_is_accepted(self):
        self.root_layout_case("true-root")

    def test_nonroot_interfaces_or_delegated_mount_are_rejected(self):
        for layout in ("domain-type", "memory-limit", "delegated-mount"):
            with self.subTest(layout=layout):
                self.root_layout_case(layout)

    def root_layout_case(self, layout):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            root = directory / "cgroup"
            root.mkdir()
            (root / "cgroup.controllers").write_text("cpu memory\n")
            if layout == "domain-type":
                (root / "cgroup.type").write_text("domain\n")
            if layout == "memory-limit":
                (root / "memory.max").write_text("max\n")
            mount_root = "/delegated" if layout == "delegated-mount" else "/"
            mountinfo = directory / "mountinfo"
            mountinfo.write_text(
                f"1 0 0:1 {mount_root} {root} rw - cgroup2 cgroup rw\n")

            def mapped_path(value):
                return (mountinfo if value == "/proc/self/mountinfo"
                        else Path(value))

            with patch.object(cgroup_setup, "ROOT", root), \
                    patch.object(cgroup_setup, "Path",
                                 side_effect=mapped_path):
                if layout == "true-root":
                    self.assertFalse((root / "cgroup.type").exists())
                    self.assertEqual(cgroup_setup.unified_root(), root)
                else:
                    with self.assertRaisesRegex(ValueError, "true cgroup"):
                        cgroup_setup.unified_root()

    def test_symlinked_root_is_rejected_before_controller_access(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            target = directory / "real"
            target.mkdir()
            alias = directory / "alias"
            alias.symlink_to(target, target_is_directory=True)
            with patch.object(cgroup_setup, "ROOT", alias):
                with self.assertRaisesRegex(ValueError, "path alias"):
                    cgroup_setup.unified_root()
            self.assertEqual(list(target.iterdir()), [])


def remove_owned(groups: dict[Path, str]) -> None:
    ordered = sorted(groups, key=lambda group: len(group.parts), reverse=True)
    for path in ordered:
        status = path.stat()
        if f"{status.st_dev}:{status.st_ino}" != groups[path]:
            raise ValueError(f"owned cgroup identity changed: {path}")
        if (path / "cgroup.procs").read_text().strip():
            raise ValueError(f"owned cgroup still has processes: {path}")
        events = dict(line.split() for line in
                      (path / "cgroup.events").read_text().splitlines())
        if events["populated"] != "0":
            raise ValueError(f"owned cgroup is still populated: {path}")
        path.rmdir()


@unittest.skipUnless("--integration" in sys.argv,
                     "requires explicit --integration")
class IntegrationTests(unittest.TestCase):
    def test_occupied_nonroot_entry_migrates_to_private_true_root(self):
        root = cgroup_setup.unified_root()
        (root / "cgroup.subtree_control").write_text("+memory\n")
        token = uuid.uuid4().hex
        fixture = root / f"storage-cgroup-fixture-{token}"
        owned = {}
        runs = []
        child = None
        helper = Path(__file__).with_name("cgroup_setup.py").resolve()
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary).resolve()
            try:
                fixture.mkdir(mode=0o700)
                status = fixture.stat()
                owned[fixture] = f"{status.st_dev}:{status.st_ino}"
                # Report the nonroot entry, then wait for stdin EOF.
                code = (
                    "import os, sys; from pathlib import Path; "
                    "(Path(sys.argv[1]) / 'cgroup.procs').write_text("
                    "str(os.getpid())); "
                    "print(Path('/proc/self/cgroup').read_text().strip(), "
                    "flush=True); sys.stdin.read()"
                )
                child = subprocess.Popen(
                    [sys.executable, "-B", "-c", code, str(fixture)],
                    stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                    stderr=subprocess.PIPE, text=True)
                ready, _, _ = select.select([child.stdout], [], [], 10)
                self.assertTrue(ready, "fixture child did not report entry")
                original = child.stdout.readline()
                self.assertEqual(original, f"0::/{fixture.name}\n")
                self.assertEqual((fixture / "cgroup.procs").read_text(),
                                 f"{child.pid}\n")
                # This is the exact old failure, not a mocked errno.
                with self.assertRaises(OSError) as failure:
                    (fixture / "cgroup.subtree_control").write_text(
                        "+memory\n")
                self.assertEqual(failure.exception.errno, errno.EBUSY)
                self.assertNotIn("memory", (fixture / "cgroup.subtree_control")
                                 .read_text().split())

                name = f"storage-cgroup-test-{token}"
                parent = root / name
                self.assertFalse(parent.exists())
                receipt = directory / "setup.json"
                environment = directory / "setup.env"
                runs.append((receipt, parent))
                command = [sys.executable, "-B", str(helper),
                           "--pid", str(child.pid), "--name", name,
                           "--receipt", str(receipt),
                           "--environment", str(environment),
                           "--parent-bytes", str(256 * 1024 ** 2),
                           "--owner-bytes", str(64 * 1024 ** 2),
                           "--bootstrap-bytes", str(128 * 1024 ** 2)]
                completed = subprocess.run(command, capture_output=True,
                                           text=True, timeout=30, check=False)
                self.assertEqual(completed.returncode, 0, completed.stderr)
                facts = json.loads(receipt.read_text())
                self.assertEqual(facts["state"], "complete")
                self.assertEqual(facts["original_cgroup"], original)
                self.assertEqual(facts["capacity_cgroup"], str(root))
                self.assertEqual(facts["unified_root"], str(root))
                status = root.stat()
                self.assertEqual(facts["root_identity"],
                                 f"{status.st_dev}:{status.st_ino}")
                self.assertEqual(Path(f"/proc/{child.pid}/cgroup").read_text(),
                                 f"0::/{name}/owner\n")
                self.assertEqual((parent / "cgroup.procs").read_text(), "")
                self.assertIn("memory", (parent / "cgroup.subtree_control")
                              .read_text().split())
                self.assertEqual(set(facts["groups"]),
                                 {str(parent), str(parent / "owner"),
                                  str(parent / "bootstrap")})
                for group, limit in ((parent, 256 * 1024 ** 2),
                                     (parent / "owner", 64 * 1024 ** 2),
                                     (parent / "bootstrap", 128 * 1024 ** 2)):
                    status = group.stat()
                    recorded = facts["groups"][str(group)]
                    self.assertEqual(recorded["identity"],
                                     f"{status.st_dev}:{status.st_ino}")
                    for field, expected in (("memory.max", str(limit)),
                                            ("memory.swap.max", "0"),
                                            ("memory.oom.group",
                                             "0" if group == parent else "1")):
                        self.assertEqual((group / field).read_text().strip(),
                                         expected)
                        self.assertEqual(recorded["limits"][field], expected)
                assignments = dict(part.split("=", 1) for part in
                                   shlex.split(environment.read_text()))
                self.assertEqual(assignments, {
                    "CGROOT": str(parent), "CAPACITY_GROUP": str(root),
                    "IDENTITY": facts["groups"][str(parent)]["identity"]})

                # A duplicate name must not reuse or change a tree.
                replay = list(command)
                replay[replay.index("--receipt") + 1] = str(
                    directory / "again.json")
                replay[replay.index("--environment") + 1] = str(
                    directory / "again.env")
                rejected = subprocess.run(replay, capture_output=True,
                                          text=True, timeout=30, check=False)
                self.assertNotEqual(rejected.returncode, 0)
                self.assertIn("already exists", rejected.stderr)
                self.assertFalse((directory / "again.json").exists())
                self.assertEqual(Path(f"/proc/{child.pid}/cgroup").read_text(),
                                 f"0::/{name}/owner\n")
                self.assertEqual(json.loads(receipt.read_text()), facts)

                # A rejected cap must leave the PID where it was.
                # Publish cleanup ownership for the partial tree.
                partial = list(command)
                partial_name = f"storage-cgroup-partial-{token}"
                partial_receipt = directory / "partial.json"
                partial_env = directory / "partial.env"
                partial[partial.index("--name") + 1] = partial_name
                partial[partial.index("--receipt") + 1] = str(partial_receipt)
                partial[partial.index("--environment") + 1] = str(partial_env)
                partial[partial.index("--parent-bytes") + 1] = str(2 ** 128)
                runs.append((partial_receipt, root / partial_name))
                failed = subprocess.run(partial, capture_output=True,
                                        text=True, timeout=30, check=False)
                self.assertNotEqual(failed.returncode, 0)
                partial_facts = json.loads(partial_receipt.read_text())
                self.assertEqual(partial_facts["state"], "initial")
                self.assertEqual(Path(f"/proc/{child.pid}/cgroup").read_text(),
                                 f"0::/{name}/owner\n")
                published = dict(part.split("=", 1) for part in
                                 shlex.split(partial_env.read_text()))
                self.assertEqual(published["CGROOT"],
                                 str(root / partial_name))
                partial_identity = partial_facts["groups"][
                    str(root / partial_name)]["identity"]
                self.assertEqual(published["IDENTITY"], partial_identity)
            finally:
                try:
                    if child is not None:
                        try:
                            _, stderr = child.communicate(timeout=10)
                        except subprocess.TimeoutExpired as error:
                            child.kill()
                            _, stderr = child.communicate(timeout=10)
                            raise TimeoutError(
                                "fixture child did not exit") from error
                        self.assertEqual(child.returncode, 0, stderr)
                finally:
                    # Only these new trees supply cleanup identities.
                    for receipt, parent in runs:
                        if receipt.exists() and receipt.stat().st_size:
                            facts = json.loads(receipt.read_text())
                            for name, recorded in facts["groups"].items():
                                path = Path(name)
                                if path not in (parent, parent / "owner",
                                                parent / "bootstrap"):
                                    raise ValueError(
                                        "cleanup path escaped owned tree")
                                owned[path] = recorded["identity"]
                    remove_owned(owned)
                self.assertFalse(fixture.exists())
                for _, parent in runs:
                    self.assertFalse(parent.exists())


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--integration", action="store_true")
    args = parser.parse_args()
    if args.integration and (sys.platform != "linux" or os.geteuid() != 0):
        parser.error("--integration requires root on Linux")
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(ArgumentTests)
    if args.integration:
        suite.addTests(unittest.defaultTestLoader.loadTestsFromTestCase(
            IntegrationTests))
    result = unittest.TextTestRunner(verbosity=2).run(suite)
    return 0 if result.wasSuccessful() else 1


if __name__ == "__main__":
    raise SystemExit(main())
