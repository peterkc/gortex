"""Refuse a pod whose observed capacity cannot support approved limits."""
import argparse
import json
import os
import shutil
from collections.abc import Mapping
from pathlib import Path


def observe(inputs: Mapping, directory: Path,
            capacity_group: Path | None = None) -> dict:
    fields = dict(line.split(":", 1) for line in
                  Path("/proc/meminfo").read_text().splitlines())
    memory = int(fields["MemTotal"].split()[0]) * 1024
    affinity = sorted(os.sched_getaffinity(0))
    root = Path("/sys/fs/cgroup")
    relative = Path("/proc/self/cgroup").read_text().strip()
    if not relative.startswith("0::/") or "\n" in relative:
        raise ValueError("unified cgroup v2 required")
    current = root / relative[4:]
    phase_group = current
    if (current.resolve() != current
            or (current != root and root not in current.parents)):
        raise ValueError("cgroup path escaped unified mount or used an alias")
    if capacity_group is not None:
        if (capacity_group.resolve() != capacity_group
                or (capacity_group != current
                    and capacity_group not in current.parents)
                or (capacity_group != root
                    and root not in capacity_group.parents)):
            raise ValueError("capacity anchor must be an actual ancestor")
    capacity_anchor = capacity_group if capacity_group is not None else current
    ceilings = []
    cpu_limits = []
    cpu_ceilings = [len(affinity)]
    while True:
        if current == capacity_anchor or current in capacity_anchor.parents:
            limit = current / "memory.max"
            if limit.exists() and limit.read_text().strip() != "max":
                ceilings.append(int(limit.read_text()))
        cpu_max = current / "cpu.max"
        raw = cpu_max.read_text().strip() if cpu_max.exists() else None
        quota = period = None
        if raw is not None:
            quota_text, period_text = raw.split()
            period = int(period_text)
            quota = None if quota_text == "max" else int(quota_text)
            if period <= 0 or (quota is not None and quota <= 0):
                raise ValueError("positive CPU quota and period required")
            if quota is not None:
                cpu_ceilings.append(quota / period)
        cpu_limits.append({"cgroup": str(current), "cpu_max": raw,
                           "quota": quota, "period": period})
        if current == root:
            break
        if root not in current.parents:
            raise ValueError("cgroup path escaped unified mount")
        current = current.parent
    return {"cpus": len(affinity), "cpu_affinity": affinity,
            "inherited_cpu_limits": cpu_limits,
            "effective_cpus": min(cpu_ceilings), "memory_bytes": memory,
            "phase_cgroup": str(phase_group),
            "capacity_cgroup": str(capacity_anchor),
            "inherited_memory_limits": ceilings,
            "available_memory_bytes": min([memory, *ceilings]),
            "free_disk_bytes": shutil.disk_usage(directory).free,
            "cgroup_controllers": (root / "cgroup.controllers").read_text(),
            "requested": inputs}


def refused(facts: Mapping, inputs: Mapping) -> bool:
    return (facts["cpus"] < inputs["vcpus"]
            or facts["effective_cpus"] < inputs["vcpus"]
            or facts["available_memory_bytes"]
            < inputs["minimum_memory_bytes"]
            or facts["free_disk_bytes"] < inputs["minimum_free_bytes"]
            or "memory" not in facts["cgroup_controllers"].split())


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inputs", type=Path, required=True)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--capacity-cgroup", type=Path)
    args = parser.parse_args()
    inputs = json.loads(args.inputs.read_text())
    facts = observe(inputs, args.directory, args.capacity_cgroup)
    facts["refused"] = refused(facts, inputs)
    with args.receipt.open("x") as stream:
        json.dump(facts, stream, indent=2)
    return 78 if facts["refused"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
