#!/usr/bin/env python3
"""Small helpers used by the generic BERT submission notebook."""

from __future__ import annotations

import argparse

from config import profile_path
from render import manifest


def build_job(
    *,
    name: str,
    image: str,
    image_sha256: str,
    run_dir: str,
    gpu: bool = False,
    settings: list[str] | None = None,
    namespace: str = "bert-benchmark",
    storage: str = "/root/.skiff/benchmarks/bert-distr-training",
    assets: str = "assets",
) -> dict:
    args = argparse.Namespace(
        name=name,
        namespace=namespace,
        image=image,
        image_sha256=image_sha256,
        python="python",
        storage_kind="hostPath",
        storage=storage,
        assets=assets,
        run_dir=run_dir,
        backend="skiff",
        config=str(profile_path(None, gpu)),
        gpu=gpu,
        set=list(settings or []),
    )
    return manifest(args)


def submit_job(job: dict) -> dict:
    from kubernetes import client, config

    try:
        config.load_incluster_config()
    except config.ConfigException:
        config.load_kube_config()
    custom = client.CustomObjectsApi()
    metadata = job["metadata"]
    return custom.create_namespaced_custom_object(
        "kubeflow.org", "v1", metadata["namespace"], "pytorchjobs", job
    )
