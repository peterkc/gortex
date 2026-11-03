"""Account-free sandbox policy fixtures, with no CLI or API calls."""
from copy import deepcopy
from decimal import Decimal
from types import MappingProxyType
import unittest

import sandbox_policy as policy


NOW = 1_793_643_060.0


def pins() -> dict:
    return {
        "cli_context": "synthetic-approved", "team_id": "team-approved",
        "image": policy.IMAGE, "harness_commit": "a" * 40,
        "approval_reference": "synthetic sandbox execution approval",
        "pricing_reference": "synthetic November 3 published-rate reference",
        "upload_reference": "synthetic 100 Mbps upload measurement",
        "download_reference": "synthetic 100 Mbps download measurement",
        "evidence_size_reference": "synthetic normal-output envelope",
        "token_size_reference": "synthetic token byte-size receipt",
        "payload_path": "/approved/inputs.tar",
        "session_path": "/approved/session.sh",
        "environment_path": "/approved/env.sh",
        "destination": "/approved/copy",
        "payload_sha256": "b" * 64, "session_sha256": "c" * 64,
        "environment_sha256": "d" * 64,
        "cpu_cores": 8, "memory_gb": 32, "disk_gb": 40,
        "timeout_minutes": 180, "max_seconds": 10800,
        "cutoff_seconds": 10680, "contingency_seconds": 3600,
        "ready_seconds": 900, "root_probe_seconds": 300,
        "setup_seconds": 1800, "bootstrap_seconds": 300,
        "analysis_seconds": 9600, "copy_seconds": 420,
        "minimum_memory_bytes": 28 * 1024 ** 3,
        "minimum_free_bytes": 32 * 1024 ** 3,
        "minimum_upload_mbps": 100, "minimum_download_mbps": 100,
        "expected_evidence_bytes": 768 * 1024 ** 2,
        "token_json_max_bytes": 256 * 1024 ** 2,
        "rate_cpu": "0.02", "rate_memory": "0.0125",
        "rate_disk": "0.0002", "budget": "3.00", "image_allowance": "0.05",
        "hourly_ceiling": "0.60",
        "network_allowlist": list(policy.BOOTSTRAP_ALLOWLIST),
    }


def details(**changes) -> dict:
    return {
        "id": "sandbox_123", "name": "owned-name", "labels": ["owned-label"],
        "docker_image": policy.IMAGE, "cpu_cores": 8, "memory_gb": 32,
        "disk_size_gb": 40, "gpu_count": 0, "timeout_minutes": 180,
        "team_id": "team-approved",
        "network_allowlist": list(policy.BOOTSTRAP_ALLOWLIST),
        **changes,
    }


class ConfigTests(unittest.TestCase):
    def test_current_shape_costs_0568_per_hour_and_2272_with_contingency(self):
        # Given approved pins, when checked, return a separate copy.
        value = pins()
        before = deepcopy(value)
        checked = policy.validate_config(MappingProxyType(value), NOW)
        self.assertEqual(checked, value)
        self.assertIsNot(checked, value)
        self.assertIsNot(checked["network_allowlist"],
                         value["network_allowlist"])
        self.assertEqual(policy.hourly(MappingProxyType(value)),
                         Decimal("0.568"))
        self.assertEqual(policy.hourly(value) * 4, Decimal("2.272"))
        self.assertEqual(policy.validate_config(checked, NOW), checked)
        self.assertEqual(value, before)
        checked["network_allowlist"].append("not-approved.invalid")
        self.assertEqual(value, before)


    def test_published_price_expiry_refuses_without_mutating_config(self):
        value = pins()
        before = deepcopy(value)
        for now in (policy.PUBLISHED_RATES_EXPIRE,
                    policy.PUBLISHED_RATES_EXPIRE + 1, float("inf"), True):
            with self.subTest(now=now), self.assertRaises(ValueError):
                policy.validate_config(value, now)
        self.assertEqual(value, before)

    def test_image_allowance_is_fixed_and_included_in_total_model(self):
        for wrong in (None, True, "0", "0.01", "0.06", "NaN"):
            with self.subTest(wrong=wrong), self.assertRaises(ValueError):
                policy.validate_config(
                    {**pins(), "image_allowance": wrong}, NOW)
        with self.assertRaises(ValueError):
            policy.validate_config({**pins(), "budget": "2.3219"}, NOW)
        checked = policy.validate_config({**pins(), "budget": "2.322"}, NOW)
        self.assertEqual(policy.hourly(checked) * 4
                         + Decimal(checked["image_allowance"]),
                         Decimal("2.322"))

    def test_hourly_rejects_invalid_rates_and_amounts_without_mutation(self):
        for field in ("rate_cpu", "rate_memory", "rate_disk", "cpu_cores",
                      "memory_gb", "disk_gb"):
            for wrong in (True, False, None, "bad", "", "-0.01", "NaN",
                          "sNaN", "Infinity", "-Infinity", float("nan"),
                          float("inf"), [], {}):
                with self.subTest(field=field, wrong=wrong):
                    value = {**pins(), field: wrong}
                    before = deepcopy(value)
                    with self.assertRaises(ValueError):
                        policy.hourly(value)
                    self.assertEqual(value, before)
        value = {**pins(), "rate_cpu": 0, "rate_memory": 0, "rate_disk": 0}
        self.assertEqual(policy.hourly(value), Decimal(0))

    def test_every_required_pin_is_required(self):
        # Given a missing pin, when validated, refuse without changes.
        for missing in pins():
            with self.subTest(missing=missing):
                value = pins()
                del value[missing]
                before = deepcopy(value)
                with self.assertRaises(ValueError):
                    policy.validate_config(value, NOW)
                self.assertEqual(value, before)
        for wrong in (None, [], "not-a-mapping", True):
            with self.subTest(config=wrong):
                with self.assertRaises(ValueError):
                    policy.validate_config(wrong, NOW)

    def test_text_and_hash_pins_refuse_placeholders_and_malformed_values(self):
        fields = (
            "cli_context", "team_id", "image", "harness_commit",
            "approval_reference", "pricing_reference", "upload_reference",
            "download_reference", "evidence_size_reference",
            "token_size_reference", "payload_path", "session_path",
            "environment_path", "destination", "payload_sha256",
            "session_sha256", "environment_sha256",
        )
        for field in fields:
            for wrong in ("", " ", None, False, 123, "PENDING: approval",
                          " REQUIRED pin", "pending-review"):
                with self.subTest(field=field, wrong=wrong):
                    value = {**pins(), field: wrong}
                    before = deepcopy(value)
                    with self.assertRaises(ValueError):
                        policy.validate_config(value, NOW)
                    self.assertEqual(value, before)
        for field, length in (("harness_commit", 40), ("payload_sha256", 64),
                              ("session_sha256", 64),
                              ("environment_sha256", 64)):
            for wrong in ("A" * length, "z" * length, "a" * (length - 1),
                          "a" * (length + 1), "a" * length + "\n"):
                with self.subTest(field=field, wrong=wrong):
                    with self.assertRaises(ValueError):
                        policy.validate_config({**pins(), field: wrong}, NOW)
        with self.assertRaises(ValueError):
            policy.validate_config({**pins(), "image": "ubuntu:22.04"}, NOW)

    def test_resources_and_timing_require_exact_supported_integer_pins(self):
        fields = (
            "cpu_cores", "memory_gb", "disk_gb", "timeout_minutes",
            "max_seconds", "cutoff_seconds", "contingency_seconds",
            "ready_seconds", "root_probe_seconds", "setup_seconds",
            "bootstrap_seconds", "analysis_seconds", "copy_seconds",
        )
        for field in fields:
            expected = pins()[field]
            for wrong in (expected + 1, expected - 1, True,
                          float(expected), str(expected)):
                with self.subTest(field=field, wrong=wrong):
                    with self.assertRaises(ValueError):
                        policy.validate_config({**pins(), field: wrong}, NOW)

    def test_capacity_transfer_and_byte_envelopes_have_explicit_bounds(self):
        for field, minimum, maximum in (
                ("minimum_memory_bytes", 28 * 1024 ** 3, None),
                ("minimum_free_bytes", 32 * 1024 ** 3, None),
                ("expected_evidence_bytes", 1,
                 8 * 1024 ** 3 + 16 * 1024 ** 2),
                ("token_json_max_bytes", 193052672, 256 * 1024 ** 2)):
            for good in (minimum, maximum if maximum is not None
                         else minimum + 1):
                with self.subTest(field=field, good=good):
                    policy.validate_config({**pins(), field: good}, NOW)
            wrong_values = [minimum - 1, True, str(minimum), float(minimum)]
            if maximum is not None:
                wrong_values.append(maximum + 1)
            for wrong in wrong_values:
                with self.subTest(field=field, wrong=wrong):
                    with self.assertRaises(ValueError):
                        policy.validate_config({**pins(), field: wrong}, NOW)
        for field in ("minimum_upload_mbps", "minimum_download_mbps"):
            for wrong in (0, -1, True, False, "100", None, float("nan"),
                          float("inf"), float("-inf")):
                with self.subTest(field=field, wrong=wrong):
                    with self.assertRaises(ValueError):
                        policy.validate_config({**pins(), field: wrong}, NOW)

    def test_published_rates_and_new_budget_caps_refuse_overspend(self):
        for field, wrong_values in (
                ("rate_cpu", ("0.021", "0", True, None, "NaN", "bad")),
                ("rate_memory", ("0.013", "0", True, None, "Infinity")),
                ("rate_disk", ("0.0003", "0", True, None, "-0.1")),
                ("budget", ("3.01", "2.2719", "2", "0", "-1", True,
                            "NaN", "Infinity", "bad")),
                ("hourly_ceiling", ("0.61", "0.5679", "0.50", "0", True,
                                    "NaN", "Infinity", "bad"))):
            for wrong in wrong_values:
                with self.subTest(field=field, wrong=wrong):
                    with self.assertRaises(ValueError):
                        policy.validate_config({**pins(), field: wrong}, NOW)
        policy.validate_config({**pins(), "budget": "2.322",
                                "hourly_ceiling": "0.568"}, NOW)

    def test_config_refuses_gpu_idle_or_attached_feature_conflicts(self):
        for field, wrong_values in (
                ("gpu_count", (1, -1, False, True, 0.0, "0", None)),
                ("gpu_type", ("A100", ["GPU"])),
                ("volumes", (["volume-id"], {"id": "volume-id"})),
                ("secrets", (["secret-id"], {"key": "value"})),
                ("env_vars", (["NAME=value"], {"NAME": "value"})),
                ("idle_timeout_minutes", (1, 0, False, "", []))):
            for wrong in wrong_values:
                with self.subTest(field=field, wrong=wrong):
                    value = {**pins(), field: wrong}
                    before = deepcopy(value)
                    with self.assertRaises(ValueError):
                        policy.validate_config(value, NOW)
                    self.assertEqual(value, before)
        for empty in (
                {"gpu_count": 0, "gpu_type": "", "volumes": [],
                 "secrets": [], "env_vars": {}, "idle_timeout_minutes": None},
                {"gpu_type": None, "volumes": None, "secrets": None,
                 "env_vars": None}):
            with self.subTest(empty=empty):
                value = {**pins(), **empty}
                before = deepcopy(value)
                self.assertEqual(policy.validate_config(value, NOW), value)
                self.assertEqual(value, before)

    def test_network_requires_exact_unique_domains_and_no_deny_rules(self):
        value = pins()
        value["network_allowlist"].reverse()
        for deny in (None, []):
            with self.subTest(deny=deny):
                candidate = {**value, "network_denylist": deny}
                before = deepcopy(candidate)
                checked = policy.validate_config(candidate, NOW)
                policy.create_arguments(candidate, "owned-name", "owned-label")
                self.assertEqual(candidate, before)
                if deny == []:
                    self.assertIsNot(checked["network_denylist"], deny)
        allow = pins()["network_allowlist"]
        for wrong in (None, [], tuple(allow), allow[:-1],
                      [*allow, "extra.invalid"], [*allow[:-1], allow[0]],
                      [*allow[:-1], "extra.invalid"], [*allow[:-1], {}]):
            with self.subTest(allow=wrong):
                candidate = {**pins(), "network_allowlist": wrong}
                before = deepcopy(candidate)
                with self.assertRaises(ValueError):
                    policy.validate_config(candidate, NOW)
                with self.assertRaises(ValueError):
                    policy.create_arguments(candidate, "name", "label")
                self.assertEqual(candidate, before)
        for wrong in (["example.invalid"], "", {}, False, ()):
            with self.subTest(deny=wrong):
                candidate = {**pins(), "network_denylist": wrong}
                with self.assertRaises(ValueError):
                    policy.validate_config(candidate, NOW)
                with self.assertRaises(ValueError):
                    policy.create_arguments(candidate, "name", "label")


class ArgumentTests(unittest.TestCase):
    def test_create_argv_pins_team_cpu_only_timeout_and_complete_egress(self):
        # Given valid pins, when building argv, emit no extra features.
        value = pins()
        before = deepcopy(value)
        expected = [
            "create",
            "ubuntu@sha256:"
            "8a2321a92b438238f93b4346fff61bc4259c99fdd4d9b52ee86f153fee0c344f",
            "--name", "owned-name", "--label", "owned-label",
            "--cpu-cores", "8", "--memory-gb", "32", "--disk-size-gb", "40",
            "--arch", "amd64", "--gpu-count", "0",
            "--timeout-minutes", "180", "--team-id", "team-approved",
            "--network-allow", "archive.ubuntu.com",
            "--network-allow", "security.ubuntu.com",
            "--network-allow", "proxy.golang.org",
            "--network-allow", "sum.golang.org",
            "--network-allow", "storage.googleapis.com",
            "--network-allow", "pypi.org",
            "--network-allow", "files.pythonhosted.org", "--yes",
        ]
        self.assertEqual(policy.create_arguments(
            MappingProxyType(value), "owned-name", "owned-label"), expected)
        value["network_allowlist"].reverse()
        self.assertEqual(policy.create_arguments(
            value, "owned-name", "owned-label"), expected)
        value["network_allowlist"].reverse()
        self.assertEqual(value, before)

    def test_create_refuses_missing_identity_team_and_network(self):
        for wrong in ("", " ", None, False, 123):
            with self.subTest(wrong=wrong):
                with self.assertRaises(ValueError):
                    policy.create_arguments(pins(), wrong, "label")
                with self.assertRaises(ValueError):
                    policy.create_arguments(pins(), "name", wrong)
                with self.assertRaises(ValueError):
                    policy.create_arguments({**pins(), "team_id": wrong},
                                            "name", "label")
        for missing in ("team_id", "network_allowlist"):
            with self.subTest(missing=missing):
                value = pins()
                del value[missing]
                with self.assertRaises(ValueError):
                    policy.create_arguments(value, "name", "label")


class IdentityTests(unittest.TestCase):
    def test_created_id_requires_one_complete_installed_cli_success_line(self):
        # Given surrounding output, accept only the exact success ID.
        for sandbox_id in ("Abcdef_7", "sandbox_123", "a" * 128):
            with self.subTest(sandbox_id=sandbox_id):
                stdout = ("Preparing sandbox\nSuccessfully created sandbox "
                          + sandbox_id + "\nStatus: PENDING\n")
                self.assertEqual(policy.created_id(stdout), sandbox_id)
        valid = "Successfully created sandbox sandbox_123"
        for stdout in ("", "sandbox_123", "id: sandbox_123", "prefix " + valid,
                       " " + valid, valid + ".", valid + " extra",
                       valid + "\n" + valid,
                       valid + "\nSuccessfully created sandbox bad!",
                       "Successfully created sandbox", valid + " "):
            with self.subTest(stdout=stdout):
                self.assertIsNone(policy.created_id(stdout))
        for sandbox_id in ("a" * 7, "a" * 129, "-abcdefg", "_abcdefg",
                           "abc.defg", "a/bcdefg", "abc defg", "ébcdefgh"):
            with self.subTest(sandbox_id=sandbox_id):
                self.assertIsNone(policy.created_id(
                    "Successfully created sandbox " + sandbox_id))

    def test_owned_requires_exact_id_name_and_label_list_not_billing(self):
        # Given proven identity, wrong billing must not prevent cleanup.
        value = details(team_id="wrong-team", labels=["other", "owned-label"])
        before = deepcopy(value)
        self.assertIs(policy.owned(MappingProxyType(value), "sandbox_123",
                                   "owned-name", "owned-label"), True)
        self.assertEqual(policy.mismatches(value, pins()), ["team_id"])
        self.assertEqual(value, before)
        for changes in ({"id": "other-id"}, {"name": "other-name"},
                        {"labels": []}, {"labels": ["other"]},
                        {"labels": None}, {"labels": "owned-label"},
                        {"labels": {"owned-label": True}}):
            with self.subTest(changes=changes):
                candidate = details(**changes)
                before = deepcopy(candidate)
                self.assertIs(policy.owned(candidate, "sandbox_123",
                                           "owned-name", "owned-label"), False)
                self.assertEqual(candidate, before)
        for field in ("id", "name", "labels"):
            with self.subTest(missing=field):
                candidate = details()
                del candidate[field]
                self.assertIs(policy.owned(candidate, "sandbox_123",
                                           "owned-name", "owned-label"), False)
        self.assertIs(policy.owned({}, "", "", ""), False)

    def test_mismatches_refuse_every_wrong_or_missing_usage_pin(self):
        config = pins()
        fields = ("docker_image", "cpu_cores", "memory_gb", "disk_size_gb",
                  "gpu_count", "timeout_minutes", "team_id",
                  "network_allowlist")
        for field in fields:
            for wrong in (None, "wrong", True, False):
                with self.subTest(field=field, wrong=wrong):
                    value = details(**{field: wrong})
                    before = deepcopy(value)
                    self.assertEqual(policy.mismatches(value, config), [field])
                    self.assertEqual(value, before)
            with self.subTest(missing=field):
                value = details()
                del value[field]
                self.assertEqual(policy.mismatches(value, config), [field])
        for field in ("cpu_cores", "memory_gb", "disk_size_gb", "gpu_count",
                      "timeout_minutes"):
            with self.subTest(field=field):
                value = details()
                value[field] += 1
                self.assertEqual(policy.mismatches(value, config), [field])
        value = {field: "wrong" for field in fields}
        value["network_denylist"] = ["example.invalid"]
        self.assertEqual(policy.mismatches(value, config),
                         [*fields, "network_denylist"])

    def test_mismatches_ignore_status_cost_and_network_domain_order(self):
        config = pins()
        before_config = deepcopy(config)
        value = details(status="PENDING", cost_per_hour="untrusted")
        value["network_allowlist"].reverse()
        for deny in (None, []):
            with self.subTest(deny=deny):
                candidate = {**value, "network_denylist": deny}
                before = deepcopy(candidate)
                self.assertEqual(policy.mismatches(
                    MappingProxyType(candidate), MappingProxyType(config)), [])
                self.assertEqual(policy.mismatches(candidate, config), [])
                self.assertEqual(candidate, before)
        self.assertEqual(policy.mismatches(value, config), [])
        allow = value["network_allowlist"]
        for wrong in (allow[:-1], [*allow[:-1], allow[0]],
                      [*allow, "extra.invalid"], tuple(allow)):
            with self.subTest(allow=wrong):
                self.assertEqual(policy.mismatches(
                    details(network_allowlist=wrong), config),
                    ["network_allowlist"])
        self.assertEqual(policy.mismatches(
            details(network_denylist=["example.invalid"]), config),
            ["network_denylist"])
        self.assertEqual(config, before_config)


if __name__ == "__main__":
    unittest.main()
