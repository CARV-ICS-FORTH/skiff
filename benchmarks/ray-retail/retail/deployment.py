"""Shared, opt-in resource contract for the ten-node CPU experiment."""

DEFAULT = {"ray_cpus": 32, "object_store_bytes": 16 * 2**30,
           "heap_bytes": 60 * 2**30, "pod_memory_bytes": 80 * 2**30,
           "physical_cores": 0, "minimum_memory_bytes": 0}


def resources(config):
    result = {**DEFAULT, **config.get("deployment", {})}
    for name, value in result.items():
        if name not in DEFAULT or type(value) is not int or value < 0:
            raise ValueError("Invalid deployment setting: " + name)
    for name in ("ray_cpus", "object_store_bytes", "heap_bytes", "pod_memory_bytes"):
        if result[name] < 1:
            raise ValueError("deployment." + name + " must be positive")
    if result["heap_bytes"] + result["object_store_bytes"] > result["pod_memory_bytes"]:
        raise ValueError("Ray heap and object store exceed the pod memory budget")
    if result["physical_cores"]:
        if config["expected_nodes"] != 10 or config["device"] != "cpu":
            raise ValueError("Whole-node experiment requires exactly 10 CPU nodes")
        if result["ray_cpus"] != result["physical_cores"] or result["physical_cores"] != 128:
            raise ValueError("Whole-node experiment requires 128 physical Ray CPUs/node")
    return result


def validate_config(config):
    if "deployment" in config:
        resources(config)


def physical_cpu_ids(allowed, root="/sys/devices/system/cpu"):
    from pathlib import Path

    cores = {}
    for cpu in sorted(allowed):
        topology = Path(root) / ("cpu%d" % cpu) / "topology"
        key = tuple((topology / name).read_text().strip()
                    for name in ("physical_package_id", "core_id"))
        cores.setdefault(key, cpu)
    return sorted(cores.values())


def bind_physical_cores(count):
    import os

    selected = physical_cpu_ids(os.sched_getaffinity(0))
    if len(selected) != count:
        raise RuntimeError("Expected %d accessible physical cores, found %d" % (count, len(selected)))
    os.sched_setaffinity(0, selected)
    return selected
