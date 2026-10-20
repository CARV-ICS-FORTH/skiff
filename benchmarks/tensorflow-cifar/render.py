"""Render a Kubernetes List (JSON is YAML) without contacting a cluster."""

from pathlib import PurePosixPath
import re
import uuid

from common import config_arguments, config_from_args, config_path, validate_workers


def manifest(args):
    if not re.fullmatch(r"[a-z](?:[a-z0-9-]{0,37}[a-z0-9])?", args.name):
        raise ValueError("name must be a DNS label of at most 39 characters")
    if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", args.namespace) or len(args.namespace) > 63:
        raise ValueError("Invalid namespace")
    if not re.fullmatch(r"[0-9a-f]{64}", args.image_sha256):
        raise ValueError("image-sha256 must contain 64 lowercase hex digits")
    for field in (config_path(args), args.data, args.run_dir):
        path = PurePosixPath(field)
        if path.is_absolute() or ".." in path.parts or not path.parts:
            raise ValueError("Config/data/run paths must stay within the shared source directory")
    if args.run_dir == args.data or args.run_dir == ".":
        raise ValueError("Use a separate results directory")
    config = config_from_args(args)
    validate_workers(config, args.workers)
    if args.stop_after_epoch is not None and not 1 <= args.stop_after_epoch <= config["epochs"]:
        raise ValueError("Invalid stop epoch")
    if args.storage_kind == "hostPath" and not PurePosixPath(args.storage).is_absolute():
        raise ValueError("hostPath storage must be absolute")
    token = uuid.uuid4().hex
    labels = {"app.kubernetes.io/name": "tensorflow-cifar", "cifar-run": args.name}
    items = [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": args.namespace}}]
    addresses = ",".join(f"{args.name}-w{r}:12345" for r in range(args.workers))
    for rank in range(args.workers):
        name = f"{args.name}-w{rank}"
        worker_labels = {**labels, "cifar-rank": str(rank)}
        items.append({
            "apiVersion": "v1", "kind": "Service",
            "metadata": {"name": name, "namespace": args.namespace},
            "spec": {"clusterIP": "None", "publishNotReadyAddresses": True,
                     "selector": worker_labels, "ports": [{"name": "grpc", "port": 12345}]},
        })
        command = [
            args.python, "-u", "run.py", "train", *config_arguments(args),
            "--data", args.data, "--output", args.run_dir, "--backend", args.backend,
            "--rank", str(rank), "--addresses", addresses, "--token", token,
        ]
        if args.resume:
            command.append("--resume")
        if args.stop_after_epoch:
            command += ["--stop-after-epoch", str(args.stop_after_epoch)]
        volume = ({"hostPath": {"path": args.storage, "type": "Directory"}}
                  if args.storage_kind == "hostPath" else {"persistentVolumeClaim": {"claimName": args.storage}})
        items.append({
            "apiVersion": "batch/v1", "kind": "Job",
            "metadata": {"name": name, "namespace": args.namespace, "labels": worker_labels},
            "spec": {
                "backoffLimit": 0,
                **({"activeDeadlineSeconds": config["timeout_seconds"]} if config["timeout_seconds"] else {}),
                "template": {"metadata": {"labels": worker_labels}, "spec": {
                    "restartPolicy": "Never", "terminationGracePeriodSeconds": 20,
                    "automountServiceAccountToken": False,
                    "affinity": {"podAntiAffinity": {"requiredDuringSchedulingIgnoredDuringExecution": [{
                        "labelSelector": {"matchLabels": labels}, "topologyKey": "kubernetes.io/hostname",
                    }]}},
                    "containers": [{
                        "name": "tensorflow", "image": args.image,
                        "imagePullPolicy": "Never" if args.backend == "skiff" else "IfNotPresent",
                        "workingDir": "/benchmark", "command": command,
                        "ports": [{"name": "grpc", "containerPort": 12345}],
                        "env": [
                            {"name": "CIFAR_NODE_NAME", "valueFrom": {"fieldRef": {"fieldPath": "spec.nodeName"}}},
                            {"name": "CIFAR_CONTAINER_SHA256", "value": args.image_sha256},
                            {"name": "PYTHONDONTWRITEBYTECODE", "value": "1"},
                        ],
                        "resources": {kind: {
                            "cpu": str(config["threads"]), "memory": f"{config['memory_gib']}Gi",
                            **({"nvidia.com/gpu": config["gpus_per_node"]} if config["device"] == "gpu" else {}),
                        } for kind in ("requests", "limits")},
                        "volumeMounts": [{"name": "workspace", "mountPath": "/benchmark"}],
                    }],
                    "volumes": [{"name": "workspace", **volume}],
                }},
            },
        })
    return {"apiVersion": "v1", "kind": "List", "items": items}
