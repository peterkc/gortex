"""Workflow coordination and capacity admission for storage checks."""

from collections.abc import Callable, Mapping, Sequence
from pathlib import Path


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
