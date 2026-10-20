#!/usr/bin/env python3
"""Render a Skiff PyTorchJob from the shared BERT profile."""

from __future__ import annotations

import re
from pathlib import PurePosixPath

from config import load_profile, profile_path


def relative_path(value: str, label: str) -> str:
    path = PurePosixPath(value)
    if path.is_absolute() or ".." in path.parts or not path.parts or value == ".":
        raise ValueError(f"{label} must stay within the shared benchmark root")
    return str(path)


def manifest(args) -> dict:
    if not re.fullmatch(r"[a-z](?:[-a-z0-9]{0,37}[a-z0-9])?", args.name):
        raise ValueError("name must be a DNS label of at most 39 characters")
    if (
        len(args.namespace) > 63
        or not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", args.namespace)
    ):
        raise ValueError("namespace must be a DNS label")
    config_name = relative_path(str(profile_path(args.config, args.gpu)), "config")
    assets = relative_path(args.assets, "assets")
    run_dir = relative_path(args.run_dir, "run-dir")
    if assets == run_dir:
        raise ValueError("Assets and results must use different directories")
    profile = load_profile(config_name, args.set, force_gpu=args.gpu)
    if args.storage_kind == "hostPath" and not PurePosixPath(args.storage).is_absolute():
        raise ValueError("hostPath storage must be absolute")
    if not re.fullmatch(r"[0-9a-f]{64}", args.image_sha256):
        raise ValueError("image-sha256 must be 64 lowercase hexadecimal characters")

    labels = {"app.kubernetes.io/name": "bert-distributed-training", "bert-run": args.name}
    volume = (
        {"hostPath": {"path": args.storage, "type": "Directory"}}
        if args.storage_kind == "hostPath"
        else {"persistentVolumeClaim": {"claimName": args.storage}}
    )
    command = [
        args.python,
        "/benchmark/launch.py",
        "--config",
        f"/benchmark/{config_name}",
        "--assets",
        f"/benchmark/{assets}",
        "--output",
        f"/benchmark/{run_dir}",
        "--backend",
        "skiff",
    ]
    if args.gpu:
        command.append("--gpu")
    for override in args.set:
        command += ["--set", override]
    resources = {
        "cpu": str(profile["cpu_threads"] * profile["processes_per_node"]),
        "memory": f"{profile['memory_gib']}Gi",
    }
    if profile["device"] == "gpu":
        resources["nvidia.com/gpu"] = profile["gpus_per_node"]
    pod_template = {
        "metadata": {"labels": labels},
        "spec": {
            "restartPolicy": "Never",
            "terminationGracePeriodSeconds": 30,
            "automountServiceAccountToken": False,
            "affinity": {
                "podAntiAffinity": {
                    "requiredDuringSchedulingIgnoredDuringExecution": [
                        {
                            "labelSelector": {"matchLabels": labels},
                            "topologyKey": "kubernetes.io/hostname",
                        }
                    ]
                }
            },
            "containers": [
                {
                    "name": "pytorch",
                    "image": args.image,
                    "imagePullPolicy": "Never" if args.backend == "skiff" else "IfNotPresent",
                    "command": command,
                    "env": [
                        {
                            "name": "BERT_NODE_NAME",
                            "valueFrom": {"fieldRef": {"fieldPath": "spec.nodeName"}},
                        },
                        {"name": "PYTHONDONTWRITEBYTECODE", "value": "1"},
                        {"name": "TOKENIZERS_PARALLELISM", "value": "false"},
                        {"name": "BERT_IMAGE_SHA256", "value": args.image_sha256},
                    ],
                    "resources": {
                        "requests": resources,
                        "limits": resources,
                    },
                    "volumeMounts": [{"name": "workspace", "mountPath": "/benchmark"}],
                }
            ],
            "volumes": [{"name": "workspace", **volume}],
        },
    }
    replicas = {
        "Master": {"replicas": 1, "restartPolicy": "Never", "template": pod_template},
    }
    if profile["nodes"] > 1:
        replicas["Worker"] = {
            "replicas": profile["nodes"] - 1,
            "restartPolicy": "Never",
            "template": pod_template,
        }
    return {
        "apiVersion": "kubeflow.org/v1",
        "kind": "PyTorchJob",
        "metadata": {
            "name": args.name,
            "namespace": args.namespace,
            "labels": labels,
            "annotations": {"bert-benchmark/image-sha256": args.image_sha256},
        },
        "spec": {
            "runPolicy": {"cleanPodPolicy": "None"},
            "pytorchReplicaSpecs": replicas,
        },
    }
