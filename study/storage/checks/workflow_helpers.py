"""Workflow coordination and capacity admission for storage checks."""

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
import re


def collect_linkdelta_tiers(
        invocations: Sequence[tuple[str, Sequence[str]]],
        run_command: Callable[[Sequence[str]], int],
        require_idle: Callable[[], None],
        record_status: Callable[[dict[str, object]], None]) -> None:
    failed = []
    for tier, command in invocations:
        try:
            code = run_command(command)
        except OSError as error:
            record_status({"tier": tier, "launch_errno": error.errno,
                           "launch_error": str(error)})
            raise
        row = {"tier": tier, "exit_code": code}
        if code < 0:
            record_status(row)
            raise RuntimeError("tier command terminated by signal: " + tier)
        try:
            require_idle()
        except (OSError, RuntimeError) as error:
            record_status({**row, "continuation_error": str(error)})
            raise
        record_status(row)
        if code:
            failed.append(tier)
    if failed:
        raise RuntimeError("tier commands failed: " + ", ".join(failed))


def require_idle_targets(root: Path,
                         expected_identity: tuple[int, int]) -> None:
    identity = root.stat()
    if (identity.st_dev, identity.st_ino) != expected_identity:
        raise RuntimeError("job cgroup identity changed between tiers")
    for child in root.iterdir():
        if child.name == "driver" or not child.is_dir():
            continue
        events = dict(line.split() for line in
                      (child / "cgroup.events").read_text().splitlines())
        if events.get("populated") != "0":
            raise RuntimeError("target cgroup is not idle: " + child.name)


def base_capacity_record(tiers: Sequence[Mapping[str, int]],
                         formula: Mapping[str, int],
                         free_bytes: int) -> dict[str, int | str]:
    fields = ("bytes", "nodes", "edges")
    if not tiers or type(free_bytes) is not int or free_bytes < 0:
        raise RuntimeError("nonnegative capacity inputs required")
    for tier in tiers:
        if any(type(tier[key]) is not int or tier[key] < 0 for key in fields):
            raise RuntimeError("nonnegative tier bytes/counts required")
    keys = ("serving_sets", "input_multiplier", "edge_bytes", "node_bytes",
            "reserve_bytes")
    if any(type(formula[key]) is not int or formula[key] <= 0 for key in keys):
        raise RuntimeError("positive capacity coefficients required")
    size, nodes, edges = (max(tier[key] for tier in tiers) for key in fields)
    required = formula["serving_sets"] * (
        formula["input_multiplier"] * size + formula["edge_bytes"] * edges
        + formula["node_bytes"] * nodes) + formula["reserve_bytes"]
    return {"S": size, "N": nodes, "E": edges,
            "required_free_bytes": required, "free_bytes": free_bytes,
            "status": "admitted" if free_bytes >= required
            else "blocked_capacity"}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise RuntimeError("duplicate policy key: " + key)
        result[key] = value
    return result


STEPS = ("bytes", "check2", "check1-plain", "check1-side", "check5-tier1",
         "check5-tier10", "check5-tier100", "check4")
BATCHES = (frozenset(("check2", "check1-plain", "check4")),
           frozenset(("check5-tier1", "check5-tier10", "check5-tier100")))


def selected_steps(text: str) -> tuple[str, ...]:
    names = tuple(part.strip() for part in text.split(","))
    if not names or any(not name for name in names):
        raise RuntimeError("nonempty named step selection required")
    if len(set(names)) != len(names):
        raise RuntimeError("duplicate step selection")
    if any(name not in STEPS for name in names):
        raise RuntimeError("unknown step selection")
    chosen = frozenset(names)
    if len(chosen) != 1 and chosen not in BATCHES:
        raise RuntimeError("step set is not approved")
    return tuple(name for name in STEPS if name in chosen)


def base_step_spec(step: str, c_variant: str) -> dict[str, object]:
    if step == "bytes":
        return {"tiers": ["tier1"], "adapters": ["a", "b", "c", "c_side"],
                "mode": "build-only", "runs": 1}
    if step == "check2":
        return {"tiers": ["tier1"], "adapters": ["c"],
                "mode": "check-only", "runs": 1}
    if step in ("check1-plain", "check1-side"):
        return {"tiers": ["tier1", "tier10"],
                "adapters": ["a", "b", "c" if step == "check1-plain"
                             else "c_side"],
                "mode": "lookup-only", "runs": 3}
    if step in ("check5-tier1", "check5-tier10", "check5-tier100"):
        if c_variant not in ("plain", "side"):
            raise RuntimeError("Check 5 requires c_variant=plain or side")
        return {"tiers": [step.removeprefix("check5-")],
                "adapters": ["a", "b", "c" if c_variant == "plain"
                             else "c_side"], "mode": "timing-only", "runs": 3}
    raise RuntimeError("unknown base step")


def measurement_jobs(steps: Sequence[str]) -> dict[str, object]:
    chosen = selected_steps(",".join(steps))
    return {"base": [step for step in chosen if step != "check4"],
            "linkdelta": "check4" in chosen, "vector": False}


def linkdelta_config_step(step: str) -> dict[str, str]:
    if step != "check4":
        raise RuntimeError("only check4 is an approved linkdelta step")
    return {"step": "check4"}


def dispatch_request(environ: Mapping[str, str]) -> dict[str, str]:
    return {"event": environ["GITHUB_EVENT_NAME"],
            "approval_id": environ["REQUEST_APPROVAL"],
            "fork_commit": environ["REQUEST_COMMIT"],
            "policy_sha256": environ["REQUEST_POLICY"],
            "release_tag": environ["REQUEST_RELEASE"],
            "steps": environ["REQUEST_STEPS"],
            "c_variant": environ["REQUEST_C_VARIANT"]}


def admit_dispatch(request, *, source, actual, policy_pin, attempt):
    if source != actual:
        raise RuntimeError("checkout differs from the workflow commit")
    if request["event"] != "workflow_dispatch":
        return
    chosen = selected_steps(request["steps"])
    for step in chosen:
        if step != "check4":
            base_step_spec(step, request["c_variant"])
    if attempt != "1":
        raise RuntimeError("no rerun: obtain approval and dispatch afresh")
    if not request["approval_id"].strip():
        raise RuntimeError("recorded exact-batch approval ID is required")
    if not re.fullmatch(r"[0-9a-f]{40}", request["fork_commit"]):
        raise RuntimeError("fork_commit must be a full lowercase commit")
    if request["fork_commit"] != actual:
        raise RuntimeError("select the reviewed workflow commit for dispatch")
    if not re.fullmatch(r"[0-9a-f]{64}", request["policy_sha256"]):
        raise RuntimeError("policy_sha256 must be a full lowercase SHA-256")
    if request["policy_sha256"] != policy_pin:
        raise RuntimeError("policy hash differs; obtain exact-value approval")
    tag = request["release_tag"]
    if (not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", tag)
            or tag == "latest"):
        raise RuntimeError("explicit fixed public fixture release required")


@dataclass(frozen=True, slots=True)
class MeasurementHost:
    system: str
    python_version: tuple[int, int]
    cpu_count: int | None
    physical_bytes: int
    private_repository: bool
    zstd_available: bool


def admit_measurement(*, event: str, role: str, checkout_status: bytes,
                      host: MeasurementHost, cpus: int,
                      job_memory_bytes: int, host_reserve_bytes: int,
                      price_cap_usd: int) -> None:
    if event != "workflow_dispatch":
        raise RuntimeError("manual approval, source or committed policy drift")
    if checkout_status:
        raise RuntimeError("measurement checkout is dirty")
    if role not in ("base", "linkdelta", "vector"):
        raise RuntimeError("unknown measurement group")
    if host.system != "Linux" or host.python_version < (3, 12):
        raise RuntimeError("Linux with Python 3.12+ required")
    if host.cpu_count != cpus:
        raise RuntimeError("actual CPU count differs from the approved venue")
    if host.physical_bytes < job_memory_bytes + host_reserve_bytes:
        raise RuntimeError(
            "host cannot supply the cap and outside-owner reserve")
    if host.private_repository or price_cap_usd != 0:
        raise RuntimeError("only the zero-cost public-fork venue is approved")
    if role in ("base", "linkdelta") and not host.zstd_available:
        raise RuntimeError("the strict public fixture fetch requires zstd")
