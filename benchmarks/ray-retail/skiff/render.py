"""Render the shared pod template for a profile; does not contact a cluster."""

import argparse
import json
from pathlib import Path
import sys

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from retail.config import add_config_arguments, load  # noqa: E402


def render(
    config_path,
    image,
    image_sha256,
    source_path,
    output_path,
    token,
    overlay=None,
    overrides=(),
):
    config = load(config_path, overlay, overrides)
    nodes = config["expected_nodes"]
    if nodes < 2:
        raise ValueError("The head/worker template needs at least two nodes")
    if config["device"] != "cpu":
        raise ValueError("This renderer qualifies CPU settings only")
    if any(not Path(p).is_absolute() for p in (image, source_path, output_path)):
        raise ValueError("Image, source and output must be absolute paths inside Skiff")
    if len(image_sha256) != 64 or any(
        c not in "0123456789abcdef" for c in image_sha256
    ):
        raise ValueError("Provide the shared SIF's lowercase SHA-256")
    # Python argv literals avoid shell interpolation of paths and tokens.
    documents = list(
        yaml.safe_load_all(Path(__file__).with_name("pods.yaml").read_text())
    )
    for document in documents:
        if document["kind"] == "Service":
            continue
        worker = document["kind"] == "Job"
        if worker:
            document["spec"]["parallelism"] = nodes - 1
            document["spec"]["completions"] = nodes - 1
        spec = document["spec"]["template"]["spec"] if worker else document["spec"]
        container = spec["containers"][0]
        container["image"] = image
        container["resources"] = {
            "requests": {"cpu": "32", "memory": "80Gi"},
            "limits": {"cpu": "32", "memory": "80Gi"},
        }
        container["env"][1]["value"] = image_sha256
        command = [
            "python3",
            "-u",
            "-m",
            "retail.cluster_node",
            "worker" if worker else "head",
            "--head-ip",
            "HEAD_IP",
            "--node-ip",
            "NODE_IP",
            "--output",
            "/report",
            "--config",
            config_path,
            "--backend",
            "skiff",
            "--token",
            token,
            "--cpus",
            "32",
            "--gpus",
            "0",
            "--object-store-bytes",
            "17179869184",
            "--heap-bytes",
            "64424509440",
            "--object-store-dir",
            "/object-store",
        ]
        if overlay:
            command += ["--overlay", overlay]
        for override in overrides or ():
            command += ["--set", override]
        head_address = (
            'socket.gethostbyname("retail-head")' if worker else 'os.environ["POD_IP"]'
        )
        entrypoint = (
            "import os,socket; os.chdir('/benchmark'); "
            f"a={command!r}; "
            f"a[a.index('HEAD_IP')]={head_address}; "
            "a[a.index('NODE_IP')]=os.environ['POD_IP']; os.execvp(a[0],a)"
        )
        container["command"] = ["python3", "-c"]
        container["args"] = [entrypoint]
        for volume in spec["volumes"]:
            if volume["name"] == "source":
                volume["hostPath"]["path"] = source_path
            elif volume["name"] == "report":
                volume["hostPath"]["path"] = output_path
            else:
                # Explicit host tmpfs mapping; emptyDir's filesystem is not assumed.
                volume.pop("emptyDir", None)
                volume["hostPath"] = {"path": "/dev/shm"}
    return {"apiVersion": "v1", "kind": "List", "items": documents}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    add_config_arguments(
        p, dest="config_path", default="configs/meluxina-20-node-cpu.json"
    )
    p.add_argument("--image", required=True)
    p.add_argument("--image-sha256", required=True)
    p.add_argument("--source-path", required=True)
    p.add_argument("--output-path", required=True)
    p.add_argument("--token", required=True)
    args = p.parse_args()
    print(json.dumps(render(**vars(args)), indent=2))


if __name__ == "__main__":
    main()
