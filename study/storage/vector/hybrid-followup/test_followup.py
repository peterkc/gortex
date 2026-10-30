"""Dry tests for sampling, paid-call bounds and response shapes."""

import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import numpy as np

import embed_voyage as voyage
import prepare_dataset as dataset
import run as runner


class FollowupTests(unittest.TestCase):
    def test_reservation_rejection_preserves_input(self):
        ledger = {"charged_or_reserved_tokens": voyage.CAP - 100,
                  "confirmed_tokens": 2, "attempted_calls": 0,
                  "pending": None}
        before = copy.deepcopy(ledger)
        with self.assertRaises(RuntimeError):
            voyage.reserve(ledger, "one", 101)
        reserved = voyage.reserve(ledger, "one", 100)
        with self.assertRaises(RuntimeError):
            voyage.reserve(reserved, "one", 1)
        self.assertEqual(ledger, before)
        settled = voyage.settle(reserved, "one", 50)
        self.assertEqual(settled["charged_or_reserved_tokens"],
                         voyage.CAP - 50)
        self.assertIsNone(settled["pending"])
        self.assertEqual(reserved["pending"]["bound"], 100)

    def test_batch_bound_and_task_identity(self):
        rows = [{"text": "x" * 40_000} for _ in range(9)]
        batches = list(voyage.batch_rows(rows))
        self.assertEqual([len(batch) for batch, _ in batches], [3, 3, 3])
        self.assertTrue(all(bound <= 110_000 for _, bound in batches))
        self.assertNotEqual(voyage.key("same", "document"),
                            voyage.key("same", "query"))

    def test_response_shape_and_duplicate_indices(self):
        vector = [1.] + [0.] * 1023
        good = {"data": [{"index": 0, "embedding": vector}]}
        actual = voyage.decode_vectors(good, 1)
        self.assertEqual(actual.shape, (1, 1024))
        self.assertEqual(actual.dtype, np.dtype("<f4"))
        with self.assertRaises(RuntimeError):
            voyage.decode_vectors({"data": good["data"] * 2}, 2)
        with self.assertRaises(RuntimeError):
            voyage.decode_vectors(
                {"data": [{"index": 0, "embedding": [0.] * 1024}]}, 1
            )

    def test_sampling_retains_target_without_mutation(self):
        rows = [{"id": "b"}, {"id": "a"}, {"id": "z"}]
        before = copy.deepcopy(rows)
        selected = dataset.sample(rows, {"z"}, 2, str)
        self.assertEqual([row["id"] for row in selected], ["a", "z"])
        self.assertEqual(rows, before)

    def test_mac_memory_rule_is_fail_closed(self):
        output = ("kern.memorystatus_level: 51\n"
                  "free swapins swapouts\n1 500 900\n2 20 0\n"
                  "kern.memorystatus_level: 56\n")
        self.assertTrue(runner.parse_mac_memory(output)["passes"])
        self.assertFalse(runner.parse_mac_memory(
            output.replace("2 20 0", "2 20 1"))["passes"])
        self.assertFalse(runner.parse_mac_memory(
            output.replace("level: 51", "level: 29"))["passes"])
        with self.assertRaises(RuntimeError):
            runner.parse_mac_memory(output.replace("2 20 0\n", ""))

    def test_memory_retry_waits_then_recovers(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(runner, "mac_memory_gate", side_effect=[
                RuntimeError("Mac memory rule failed; accuracy stopped"),
                {"passes": True},
            ]) as sample, patch.object(
                runner.subprocess, "check_output",
                return_value="wait observation",
            ) as wait:
                result = runner.mac_memory_retry(Path(directory), "job")
                self.assertTrue(result["passes"])
                self.assertEqual(sample.call_count, 2)
                wait.assert_called_once_with(
                    ["vm_stat", "-c", "2", "300"], text=True)
                waits = list(Path(directory).glob("*wait*.log"))
                self.assertEqual(len(waits), 1)

    def test_memory_retry_stops_after_six_waits(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(runner, "mac_memory_gate", side_effect=
                              RuntimeError("Mac memory rule failed; "
                                           "accuracy stopped")) as sample:
                with patch.object(runner.subprocess, "check_output",
                                  return_value="wait observation") as wait:
                    with self.assertRaises(RuntimeError):
                        runner.mac_memory_retry(Path(directory), "job")
                    self.assertEqual(sample.call_count, 7)
                    self.assertEqual(wait.call_count, 6)

    def test_memory_retry_does_not_hide_other_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            with patch.object(runner, "mac_memory_gate",
                              side_effect=RuntimeError("bad format")):
                with patch.object(runner.subprocess, "check_output") as wait:
                    with self.assertRaisesRegex(RuntimeError, "bad format"):
                        runner.mac_memory_retry(Path(directory), "job")
                    wait.assert_not_called()

    def test_runtime_manifest_excludes_controller(self):
        paths = runner.runtime_paths()
        self.assertNotIn(runner.ROOT / "run.py", paths)
        self.assertIn(runner.ROOT / "go.mod", paths)
        self.assertTrue(all(not path.name.endswith("_test.go")
                            for path in paths))

    def test_portable_job_grids_do_not_execute(self):
        timing = runner.timing_jobs(Path("timing"))
        blocks = [job for job in timing if job.kind == "timing"]
        self.assertEqual(len(blocks), 54)
        self.assertEqual(len({job.name for job in timing}), len(timing))
        accuracy = runner.accuracy_jobs(Path("ann"))
        self.assertEqual(len({job.name for job in accuracy}), len(accuracy))
        i8_builds = [job for job in accuracy
                     if "-method" in job.command
                     and job.command[job.command.index("-method") + 1] == "i8"
                     and "build" in job.command]
        self.assertEqual(len(i8_builds), 40)
        latency = runner.latency_jobs(Path("ann"))
        self.assertEqual(len(latency), 240)
        self.assertTrue(all(job.kind == "timing" for job in latency))

    def test_generated_latency_pairs_exact_and_retains_each_recipe(self):
        with patch.object(runner, "WORK", Path("/fixture/hybrid-followup")):
            jobs = runner.latency_jobs(Path("ann"), generated=True)
        self.assertEqual(len(jobs), 54)
        self.assertEqual(len({job.name for job in jobs}), 54)
        self.assertEqual(sum(j.kind == "preparation" for j in jobs), 6)
        timed = [j for j in jobs if j.kind == "timing"]
        self.assertEqual(len(timed), 48)
        recipes = set()
        for index in range(0, len(timed), 6):
            group = timed[index:index + 6]
            flags = [dict(zip(j.command[1::2], j.command[2::2]))
                     for j in group]
            self.assertEqual([f["-method"] for f in flags[::2]], ["exact"] * 3)
            self.assertEqual(len({f["-dataset"] for f in flags}), 1)
            self.assertEqual(len({j.name for j in group}), 6)
            self.assertTrue(all(j.artifact is None for j in group))
            for a, b in zip(group[::2], group[1::2]):
                self.assertEqual(a.name, b.name + "-reference")
            b = flags[1]
            self.assertTrue(all(f == b for f in flags[1::2]))
            recipes.add((b["-method"], b["-m"], b["-ef"], b["-workers"]))
        self.assertEqual(recipes, {("int8", "16", "512", str(w))
                                   for w in (1, 4, 8)} |
                                  {("f32", "32", "1024", "1")})

    def test_exact_text_spacing_and_chunk_ids(self):
        row = {"id": "symbol", "kind": "function", "name": "F",
               "signature": "", "file_path": "beads/f.go"}
        got = dataset.corpus_rows([row], {})
        self.assertEqual(got[0]["text"], "function F  beads/f.go")
        chunks = {"beads::symbol": {"chunks": [
            {"Text": "a", "WindowIndex": 0},
            {"Text": "b", "WindowIndex": 1},
        ]}}
        got = dataset.corpus_rows([row], chunks)
        self.assertEqual([item["id"] for item in got],
                         ["beads::symbol#chunk0", "beads::symbol#chunk1"])
        self.assertEqual([item["text"] for item in got],
                         ["function F a", "function F b"])


if __name__ == "__main__":
    unittest.main()
