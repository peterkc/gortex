"""C2 root/download admission scenarios; all evidence is synthetic.

These unittest fixtures do not rent, inspect a provider or run inference.
"""
from copy import deepcopy
from hashlib import sha256
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import admission


class AdmissionC2Tests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.evidence = self.root / "root-evidence.txt"
        self.raw = b"synthetic fixture, not provider root proof\n"
        self.evidence.write_bytes(self.raw)
        identity = {"provider": "nebius", "cloud_id": "cpu-d3_8vcpu-32gb",
                    "location": "FI", "image": "approved-image"}
        self.config = {
            **identity, "ready_seconds": 900, "copy_seconds": 420,
            "minimum_download_mbps": 100,
            "download_reference": "synthetic measured downlink reference",
            "expected_evidence_bytes": 768 * 1024 ** 2,
            "evidence_size_reference": "synthetic normal-volume review",
            "cgroup_root": {
                **identity, "mode": "prior-evidence",
                "source_reference": "synthetic reviewed root receipt",
                "evidence_path": str(self.evidence),
                "evidence_sha256": sha256(self.raw).hexdigest(),
            },
        }

    def test_prior_evidence_exact_identity_and_hash_returns_readiness(self):
        # Given matching bound bytes, repeated checks preserve the config.
        before = deepcopy(self.config)
        for _ in range(2):
            self.assertEqual(admission.root_probe_seconds(self.config), 900)
        self.assertEqual(self.config, before)
        self.assertEqual(self.evidence.read_bytes(), self.raw)

    def test_prior_evidence_requires_exact_complete_identity(self):
        # When any identity field is absent or different, refuse the proof.
        for name in ("provider", "cloud_id", "location", "image"):
            for replacement in (None, "", "different"):
                with self.subTest(name=name, replacement=replacement):
                    config = deepcopy(self.config)
                    if replacement is None:
                        del config["cgroup_root"][name]
                    else:
                        config["cgroup_root"][name] = replacement
                    with self.assertRaisesRegex(ValueError, "identity"):
                        admission.root_probe_seconds(config)
            for replacement in (None, "", True):
                with self.subTest(config_name=name, value=replacement):
                    config = deepcopy(self.config)
                    config[name] = replacement
                    config["cgroup_root"][name] = replacement
                    with self.assertRaisesRegex(ValueError, "identity"):
                        admission.root_probe_seconds(config)

    def test_prior_evidence_requires_reference_and_sha256(self):
        for name in ("source_reference", "evidence_path", "evidence_sha256"):
            for replacement in (None, "", " ", True):
                with self.subTest(name=name, replacement=replacement):
                    config = deepcopy(self.config)
                    if replacement is None:
                        del config["cgroup_root"][name]
                    else:
                        config["cgroup_root"][name] = replacement
                    with self.assertRaises(ValueError):
                        admission.root_probe_seconds(config)
        for pin in ("a" * 63, "G" * 64, "0" * 64):
            with self.subTest(pin=pin):
                config = deepcopy(self.config)
                config["cgroup_root"]["evidence_sha256"] = pin
                with self.assertRaises(ValueError):
                    admission.root_probe_seconds(config)

    def test_prior_evidence_rejects_missing_nonregular_and_aliased_paths(self):
        alias = self.root / "alias"
        alias.symlink_to(self.evidence)
        directory_alias = self.root / "directory-alias"
        directory_alias.symlink_to(self.root, target_is_directory=True)
        names = (str(self.root / "missing"), str(self.root), str(alias),
                 str(directory_alias / self.evidence.name),
                 str(self.root) + "/./" + self.evidence.name,
                 self.evidence.name)
        for name in names:
            with self.subTest(path=name):
                config = deepcopy(self.config)
                config["cgroup_root"]["evidence_path"] = name
                with self.assertRaisesRegex(ValueError, "canonical/regular"):
                    admission.root_probe_seconds(config)

    def test_prior_evidence_rechecks_changes_removal_and_restoration(self):
        # Given accepted bytes, updates/removal reject until restored.
        self.assertEqual(admission.root_probe_seconds(self.config), 900)
        self.evidence.write_bytes(b"changed fixture bytes")
        with self.assertRaisesRegex(ValueError, "hash differs"):
            admission.root_probe_seconds(self.config)
        self.evidence.unlink()
        with self.assertRaisesRegex(ValueError, "canonical/regular"):
            admission.root_probe_seconds(self.config)
        self.evidence.write_bytes(self.raw)
        self.assertEqual(admission.root_probe_seconds(self.config), 900)

    def test_prior_evidence_requires_positive_integer_readiness(self):
        for seconds in (None, True, 0, -1, 1.5, "900"):
            with self.subTest(seconds=seconds):
                config = deepcopy(self.config)
                config["ready_seconds"] = seconds
                with self.assertRaisesRegex(ValueError, "ready_seconds"):
                    admission.root_probe_seconds(config)

    def test_first_in_pod_check_accepts_explicitly_approved_300_seconds(self):
        # This test reference does not authorize any real rental.
        self.config["cgroup_root"] = {
            "mode": "first-in-pod-check", "probe_seconds": 300,
            "approval_reference": "synthetic user approval of uncertainty",
        }
        before = deepcopy(self.config)
        self.assertEqual(admission.root_probe_seconds(self.config), 300)
        self.config["cgroup_root"]["probe_seconds"] = 1
        self.assertEqual(admission.root_probe_seconds(self.config), 1)
        self.config["cgroup_root"]["probe_seconds"] = 300
        self.assertEqual(self.config, before)

    def test_first_in_pod_check_requires_approval_and_bounded_seconds(self):
        fallback = {"mode": "first-in-pod-check", "probe_seconds": 300,
                    "approval_reference": "synthetic uncertainty approval"}
        for seconds in (None, True, False, 0, -1, 301, 1.5, "300"):
            with self.subTest(seconds=seconds):
                config = deepcopy(self.config)
                config["cgroup_root"] = {**fallback,
                                         "probe_seconds": seconds}
                with self.assertRaisesRegex(ValueError, "approval"):
                    admission.root_probe_seconds(config)
        for reference in (None, "", " ", True):
            with self.subTest(reference=reference):
                config = deepcopy(self.config)
                config["cgroup_root"] = dict(fallback)
                if reference is None:
                    del config["cgroup_root"]["approval_reference"]
                else:
                    config["cgroup_root"]["approval_reference"] = reference
                with self.assertRaisesRegex(ValueError, "approval"):
                    admission.root_probe_seconds(config)

    def test_root_check_rejects_missing_non_dict_or_unknown_mode(self):
        for root in (None, [], "prior-evidence", {}, {"mode": "unknown"}):
            with self.subTest(root=root):
                config = {**self.config, "cgroup_root": root}
                with self.assertRaises(ValueError):
                    admission.root_probe_seconds(config)
        config = dict(self.config)
        del config["cgroup_root"]
        with self.assertRaises(ValueError):
            admission.root_probe_seconds(config)

    def test_download_768_mib_fits_100_mbps_but_not_10_mbps(self):
        # Given 420 seconds, 100 Mbps fits at 111; 10 Mbps needs 836.
        before = deepcopy(self.config)
        size = self.config["expected_evidence_bytes"]
        self.assertEqual(admission.check_download(self.config, size, 420), 111)
        self.assertEqual(self.config, before)
        slow = {**self.config, "minimum_download_mbps": 10}
        with self.assertRaisesRegex(ValueError, "copy window"):
            admission.check_download(slow, size, 420)

    def test_download_accepts_exact_duration_and_refuses_one_second_less(self):
        # Given 10,000,001 bytes at 100 Mbps, the formula needs 32 seconds.
        self.assertEqual(admission.check_download(
            self.config, 10_000_001, 32), 32)
        with self.assertRaisesRegex(ValueError, "copy window"):
            admission.check_download(self.config, 10_000_001, 31)

    def test_download_rejects_invalid_rates_directly(self):
        for rate in (None, True, False, 0, -1, float("nan"),
                     float("inf"), "100"):
            with self.subTest(rate=rate):
                config = {**self.config, "minimum_download_mbps": rate}
                with self.assertRaises(ValueError):
                    admission.check_download(config, 1, 420)

    def test_download_requires_bounded_positive_integer_expected_bytes(self):
        maximum = admission.MAX_PAYLOAD + admission.TAR_OVERHEAD
        for size in (None, True, False, 0, -1, 1.5, "768", maximum + 1):
            with self.subTest(size=size):
                config = {**self.config, "expected_evidence_bytes": size}
                with self.assertRaisesRegex(ValueError, "envelope"):
                    admission.check_download(config, 1, 420)
        for size in (1, maximum):
            with self.subTest(accepted_envelope=size):
                config = {**self.config, "expected_evidence_bytes": size}
                self.assertEqual(admission.check_download(config, 1, 31), 31)

    def test_download_actual_bytes_are_not_capped_by_normal_estimate(self):
        config = {**self.config, "expected_evidence_bytes": 1}
        self.assertEqual(admission.check_download(
            config, 768 * 1024 ** 2, 420), 111)

    def test_download_rejects_invalid_actual_bytes_or_window(self):
        for value in (None, True, False, 0, -1, 1.5, "420"):
            with self.subTest(size=value):
                with self.assertRaisesRegex(ValueError, "positive integers"):
                    admission.check_download(self.config, value, 420)
            with self.subTest(seconds=value):
                with self.assertRaisesRegex(ValueError, "positive integers"):
                    admission.check_download(self.config, 1, value)

    def test_actual_bytes_refuse_absolute_archive_bound_before_transfer(self):
        # The normal forecast is not a cap; the artifact's hard bound is.
        config = {**self.config, "minimum_download_mbps": 1_000_000}
        maximum = admission.MAX_PAYLOAD + admission.TAR_OVERHEAD
        with self.assertRaisesRegex(ValueError, "archive bound"):
            admission.check_download(config, maximum + 1, 420)

    def test_download_requires_nonempty_rate_and_size_references(self):
        for name in ("download_reference", "evidence_size_reference"):
            for reference in (None, "", " ", True):
                with self.subTest(name=name, reference=reference):
                    config = dict(self.config)
                    if reference is None:
                        del config[name]
                    else:
                        config[name] = reference
                    with self.assertRaisesRegex(ValueError, "references"):
                        admission.check_download(config, 1, 420)

    def test_upload_seconds_preserves_formula_and_rejects_bad_types(self):
        for size, expected in ((0, 30), (9_999_999, 31),
                               (10_000_000, 31), (10_000_001, 32)):
            with self.subTest(size=size):
                self.assertEqual(admission.upload_seconds(size, 100), expected)
        for size, rate in ((True, 100), (1.5, 100), (-1, 100),
                           (1, True), (1, None), (1, "100")):
            with self.subTest(size=size, rate=rate):
                with self.assertRaises(ValueError):
                    admission.upload_seconds(size, rate)

    def test_validate_refuses_unproven_root_before_checkout_or_transport(self):
        # Invalid proof must stop this pre-rent boundary immediately.
        config = deepcopy(self.config)
        config["cgroup_root"]["evidence_sha256"] = "0" * 64
        with patch.object(admission.subprocess, "check_output") as checkout:
            with self.assertRaisesRegex(ValueError, "hash differs"):
                admission.validate(config)
        checkout.assert_not_called()

    def test_validate_refuses_infeasible_download_before_checkout(self):
        # The root check uses an actual tiny file, never a provider claim.
        config = {**self.config, "minimum_download_mbps": 10}
        with patch.object(admission.subprocess, "check_output") as checkout:
            with self.assertRaisesRegex(ValueError, "copy window"):
                admission.validate(config)
        checkout.assert_not_called()


if __name__ == "__main__":
    unittest.main()
