"""Pure policy for the approved CPU sandbox, with no CLI calls or clocks."""
from collections.abc import Mapping
from decimal import Decimal, DecimalException
import re


IMAGE = (
    "ubuntu@sha256:"
    "8a2321a92b438238f93b4346fff61bc4259c99fdd4d9b52ee86f153fee0c344f"
)
BOOTSTRAP_ALLOWLIST = (
    "archive.ubuntu.com",
    "security.ubuntu.com",
    "proxy.golang.org",
    "sum.golang.org",
    "storage.googleapis.com",
    "pypi.org",
    "files.pythonhosted.org",
)
# The same frozen token envelope as admission.TOKEN_JSON_BOUND.
TOKEN_JSON_BOUND = (
    (900 * 2048 + 48 * 4096 + 10 * 8192) * 3 * 30 + 2 * 1024 ** 2
)
REQUIRED_TEXT = (
    "cli_context", "team_id", "image", "harness_commit",
    "approval_reference", "pricing_reference", "upload_reference",
    "download_reference", "evidence_size_reference", "token_size_reference",
    "payload_path", "session_path", "environment_path", "destination",
)
CONFIG_PINS = (
    ("cpu_cores", 8), ("memory_gb", 32), ("disk_gb", 40),
    ("timeout_minutes", 180), ("max_seconds", 10800),
    ("cutoff_seconds", 10680), ("contingency_seconds", 3600),
    ("ready_seconds", 900), ("root_probe_seconds", 300),
    ("setup_seconds", 1800), ("bootstrap_seconds", 300),
    ("analysis_seconds", 9600), ("copy_seconds", 420),
)
# Public prices are stated valid through December22,2026. UTC is conservative.
PUBLISHED_RATES_EXPIRE = 1797984000
RATE_PINS = (
    ("rate_cpu", Decimal("0.02")),
    ("rate_memory", Decimal("0.0125")),
    ("rate_disk", Decimal("0.0002")),
)


def _number(value: object, field: str) -> Decimal:
    if isinstance(value, bool) or not isinstance(
            value, (str, int, float, Decimal)):
        raise ValueError(f"{field} must be a finite nonnegative number")
    try:
        number = Decimal(str(value))
    except DecimalException as error:
        raise ValueError(f"{field} must be a number") from error
    if not number.is_finite() or number < 0:
        raise ValueError(f"{field} must be finite and nonnegative")
    return number


def _required_text(value: object, field: str) -> str:
    if (not isinstance(value, str) or not value.strip()
            or value.lstrip().upper().startswith(("PENDING", "REQUIRED"))):
        raise ValueError(f"{field} needs nonempty approved text")
    return value


def _allowlist_matches(value: object) -> bool:
    return (
        isinstance(value, list)
        and len(value) == len(BOOTSTRAP_ALLOWLIST)
        and all(isinstance(domain, str) for domain in value)
        and set(value) == set(BOOTSTRAP_ALLOWLIST)
    )


def _denylist_empty(value: object) -> bool:
    return value is None or isinstance(value, list) and not value


def hourly(config: Mapping) -> Decimal:
    """Compute CPU, memory and disk cost from nonnegative finite inputs."""
    if not isinstance(config, Mapping):
        raise ValueError("configuration must be a mapping")
    total = Decimal(0)
    try:
        for rate, amount in (("rate_cpu", "cpu_cores"),
                             ("rate_memory", "memory_gb"),
                             ("rate_disk", "disk_gb")):
            total += (_number(config.get(rate), rate)
                      * _number(config.get(amount), amount))
    except DecimalException as error:
        raise ValueError("hourly cost is outside the decimal range") from error
    if not total.is_finite():
        raise ValueError("hourly cost must be finite")
    return total


def validate_config(value: Mapping, now: float) -> dict:
    """Check published-price policy without I/O or quote-age checks.

    ``now`` refuses use after the published price window; no live quote input.
    A missing, None or empty-list denylist means no deny rules.
    """
    if not isinstance(value, Mapping):
        raise ValueError("configuration must be a mapping")
    if _number(now, "current_epoch") >= PUBLISHED_RATES_EXPIRE:
        raise ValueError("published sandbox prices require renewed review")
    for field in REQUIRED_TEXT:
        _required_text(value.get(field), field)
    if value["image"] != IMAGE:
        raise ValueError("image differs from the approved amd64 digest")
    for field, length in (("harness_commit", 40), ("payload_sha256", 64),
                          ("session_sha256", 64),
                          ("environment_sha256", 64)):
        pin = _required_text(value.get(field), field)
        if re.fullmatch(r"[0-9a-f]{" + str(length) + r"}", pin) is None:
            raise ValueError(f"{field} must be {length} lowercase hex digits")
    for field, expected in CONFIG_PINS:
        if type(value.get(field)) is not int or value[field] != expected:
            raise ValueError(f"{field} must equal {expected}")
    for field, minimum in (("minimum_memory_bytes", 28 * 1024 ** 3),
                           ("minimum_free_bytes", 32 * 1024 ** 3)):
        if type(value.get(field)) is not int or value[field] < minimum:
            raise ValueError(f"{field} must be at least {minimum}")
    for field in ("minimum_upload_mbps", "minimum_download_mbps"):
        if (type(value.get(field)) not in (int, float)
                or _number(value[field], field) <= 0):
            raise ValueError(f"{field} must be positive and finite")
    for field, minimum, maximum in (
            ("expected_evidence_bytes", 1, 8 * 1024 ** 3 + 16 * 1024 ** 2),
            ("token_json_max_bytes", TOKEN_JSON_BOUND, 256 * 1024 ** 2)):
        if (type(value.get(field)) is not int
                or not minimum <= value[field] <= maximum):
            raise ValueError(f"{field} is outside the approved byte envelope")
    for field, expected in RATE_PINS:
        if _number(value.get(field), field) != expected:
            raise ValueError(f"{field} differs from the published rate")
    cost = hourly(value)
    ceiling = _number(value.get("hourly_ceiling"), "hourly_ceiling")
    budget = _number(value.get("budget"), "budget")
    if not 0 < cost <= ceiling <= Decimal("0.60"):
        raise ValueError("hourly cost exceeds the approved ceiling")
    if not 0 < budget <= Decimal("3.00"):
        raise ValueError("budget must be positive and at most 3.00")
    allowance = _number(value.get("image_allowance"), "image_allowance")
    if allowance != Decimal("0.05"):
        raise ValueError("image_allowance must equal the approved 0.05")
    seconds = value["max_seconds"] + value["contingency_seconds"]
    if cost * seconds / 3600 + allowance > budget:
        raise ValueError("resources plus image allowance exceed budget")
    if not _allowlist_matches(value.get("network_allowlist")):
        raise ValueError("network_allowlist must contain each approved domain")
    if not _denylist_empty(value.get("network_denylist")):
        raise ValueError("network_denylist must have no deny rules")
    if "gpu_count" in value and (
            type(value["gpu_count"]) is not int or value["gpu_count"] != 0):
        raise ValueError("gpu_count must be integer zero when supplied")
    for field in ("gpu_type", "volumes", "secrets", "env_vars"):
        if value.get(field):
            raise ValueError(f"{field} must be omitted or empty")
    if value.get("idle_timeout_minutes") is not None:
        raise ValueError("idle_timeout_minutes must be omitted or None")
    config = dict(value)
    config["network_allowlist"] = list(value["network_allowlist"])
    if isinstance(value.get("network_denylist"), list):
        config["network_denylist"] = []
    return config


def create_arguments(config: Mapping, name: str, label: str) -> list[str]:
    """Build argv from validated pins; recheck egress and billing team."""
    if not isinstance(config, Mapping):
        raise ValueError("configuration must be a mapping")
    for field, text in (("name", name), ("label", label)):
        if not isinstance(text, str) or not text.strip():
            raise ValueError(f"{field} must be nonempty text")
    team = _required_text(config.get("team_id"), "team_id")
    if not _allowlist_matches(config.get("network_allowlist")):
        raise ValueError("network_allowlist must contain each approved domain")
    if not _denylist_empty(config.get("network_denylist")):
        raise ValueError("network_denylist must have no deny rules")
    arguments = [
        "create", IMAGE, "--name", name, "--label", label,
        "--cpu-cores", "8", "--memory-gb", "32", "--disk-size-gb", "40",
        "--arch", "amd64", "--gpu-count", "0", "--timeout-minutes", "180",
        "--team-id", team,
    ]
    for domain in BOOTSTRAP_ALLOWLIST:
        arguments.extend(("--network-allow", domain))
    return [*arguments, "--yes"]


def created_id(stdout: str) -> str | None:
    """Accept one exact installed-CLI success line with a bounded ID."""
    lines = [line for line in stdout.splitlines()
             if line.startswith("Successfully created sandbox")]
    if len(lines) != 1:
        return None
    match = re.fullmatch(
        r"Successfully created sandbox ([A-Za-z0-9][A-Za-z0-9_-]{7,127})",
        lines[0],
    )
    return match[1] if match else None


def owned(document: Mapping, sandbox_id: str, name: str, label: str) -> bool:
    """Prove cleanup ownership even when the billing team is wrong."""
    if not isinstance(document, Mapping):
        return False
    if any(not isinstance(text, str) or not text.strip()
           for text in (sandbox_id, name, label)):
        return False
    labels = document.get("labels")
    return (document.get("id") == sandbox_id
            and document.get("name") == name
            and isinstance(labels, list) and label in labels)


def mismatches(document: Mapping, config: Mapping) -> list[str]:
    """Report get-detail fields that forbid use, including billing."""
    if not isinstance(document, Mapping) or not isinstance(config, Mapping):
        raise ValueError("details and configuration must be mappings")
    team = _required_text(config.get("team_id"), "team_id")
    fields = []
    if document.get("docker_image") != IMAGE:
        fields.append("docker_image")
    for field, expected in (("cpu_cores", 8), ("memory_gb", 32),
                            ("disk_size_gb", 40), ("gpu_count", 0),
                            ("timeout_minutes", 180)):
        actual = document.get(field)
        if isinstance(actual, bool) or actual != expected:
            fields.append(field)
    if document.get("team_id") != team:
        fields.append("team_id")
    if not _allowlist_matches(document.get("network_allowlist")):
        fields.append("network_allowlist")
    if not _denylist_empty(document.get("network_denylist")):
        fields.append("network_denylist")
    return fields
