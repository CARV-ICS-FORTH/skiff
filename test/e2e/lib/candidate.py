#!/usr/bin/env python3
"""Record and validate the image actually exercised by the qualification run."""

import hashlib
import json
import pathlib
import re
import sys


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def validate(record):
    manifest = record.get("manifest") or {}
    if manifest.get("git_dirty") is not False or not re.fullmatch(r"[0-9a-f]{40}", manifest.get("git_commit", "")):
        raise ValueError("candidate manifest does not identify a clean source commit")
    if not re.fullmatch(r"[0-9a-f]{64}", manifest.get("tar_sha256", "")):
        raise ValueError("candidate archive digest missing")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", manifest.get("builder_image_id", "")):
        raise ValueError("builder image identity missing")
    if not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", manifest.get("go_version", "")):
        raise ValueError("Go toolchain version missing")
    if not record.get("fixture_image_tag") or not re.fullmatch(r"[0-9a-f]{64}", record.get("fixture_sif_sha256", "")):
        raise ValueError("fixture identity missing")
    if not re.fullmatch(r"[0-9]+", record.get("job_id", "")):
        raise ValueError("Slurm job identity missing")
    nodes = record.get("nodes") or []
    names = {n.get("name") for n in nodes}
    if len(nodes) != 2 or len(names) != 2 or "" in names or None in names:
        raise ValueError("both runtime nodes must be recorded, under names of their own")
    if len({n.get("architecture") for n in nodes}) != 1:
        raise ValueError("the runtime nodes differ in architecture")
    # The image that was uploaded must be the nodes' architecture (the manifest names its platform).
    expected = {"linux/amd64": "x86_64", "linux/arm64": "aarch64"}.get(manifest.get("platform", ""))
    for node in nodes:
        if node.get("architecture") not in ("aarch64", "x86_64"):
            raise ValueError("unqualified runtime architecture")
        if expected and node.get("architecture") != expected:
            raise ValueError("the image is built for %s and the node is %s" % (manifest.get("platform"), node.get("architecture")))
        if node.get("sif_sha256") != record.get("sif_sha256"):
            raise ValueError("node image digest mismatch")
        if node.get("kubelet_version") != manifest.get("version"):
            raise ValueError("node runtime version mismatch")


def build(args):
    manifest_path, sif_path, fixture_path, fixture_env, nodes_path, out_path, run_id, job_id = args
    manifest = json.loads(pathlib.Path(manifest_path).read_text())
    fixture_text = pathlib.Path(fixture_env).read_text()
    match = re.search(r'^export FIXTURE_IMAGE_TAG="([^"]+)"$', fixture_text, re.M)
    fixture_tag = match.group(1) if match else ""
    nodes = []
    for line in pathlib.Path(nodes_path).read_text().splitlines():
        name, arch, sif_sha, version = line.split("\t")
        nodes.append(dict(name=name, architecture=arch, sif_sha256=sif_sha, kubelet_version=version))
    record = dict(run_id=run_id, job_id=job_id, manifest=manifest,
                  sif_sha256=digest(sif_path), fixture_sif_sha256=digest(fixture_path),
                  fixture_image_tag=fixture_tag, nodes=nodes)
    pathlib.Path(out_path).write_text(json.dumps(record, indent=2) + "\n")
    validate(record)


def self_test():
    sha = "a" * 64
    record = dict(job_id="12", sif_sha256=sha, fixture_sif_sha256=sha,
                  fixture_image_tag="test", manifest=dict(git_dirty=False, git_commit="b" * 40,
                  tar_sha256=sha, version="test",
                  builder_image_id="sha256:" + sha, go_version="go1.26.8"),
                  nodes=[dict(name=name, architecture="aarch64", sif_sha256=sha,
                              kubelet_version="test") for name in ("controller", "node")])
    validate(record)
    # Any two distinct node names, on either qualified architecture, with an image built for it.
    other = json.loads(json.dumps(record))
    other["manifest"]["platform"] = "linux/amd64"
    for node, name in zip(other["nodes"], ("k8s-node1", "k8s-node2")):
        node.update(name=name, architecture="x86_64")
    validate(other)
    for mutation in (
        lambda r: r["manifest"].update(git_dirty=True),
        lambda r: r["manifest"].pop("builder_image_id"),
        lambda r: r["manifest"].update(go_version="1.26"),
        lambda r: r["nodes"][1].update(sif_sha256="c" * 64),
        lambda r: r["nodes"][0].update(kubelet_version="wrong"),
        lambda r: r["nodes"].pop(),
        lambda r: r["nodes"][1].update(name=r["nodes"][0]["name"]),
        lambda r: r["nodes"][1].update(architecture="x86_64"),
        lambda r: r["nodes"][0].update(architecture="riscv64") or r["nodes"][1].update(architecture="riscv64"),
        lambda r: r["manifest"].update(platform="linux/amd64"),
        lambda r: r.update(fixture_image_tag=""),
        lambda r: r.update(job_id=""),
    ):
        broken = json.loads(json.dumps(record))
        mutation(broken)
        try:
            validate(broken)
        except ValueError:
            continue
        raise AssertionError("invalid candidate evidence was accepted")


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        self_test()
    elif len(sys.argv) == 9:
        build(sys.argv[1:])
    else:
        raise SystemExit("usage: candidate.py MANIFEST SIF FIXTURE_SIF FIXTURE_ENV NODES_TSV OUTPUT RUN_ID JOB_ID")
