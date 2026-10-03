#!/usr/bin/env bash
# The default storage manifest the controller installs (images/skiff/storage.yaml):
# what the entrypoint does to it (replace the placeholder with the job's volume directory)
# gives a configuration the provisioner can read, and the settings the design depends on
# are there (one default class, Delete). The entrypoint's own sed is the one used here.
set -euo pipefail
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
manifest="$repo_dir/images/skiff/storage.yaml"
entrypoint="$repo_dir/images/skiff/entrypoint.sh"
fail() { echo "storage-manifest-test: $*" >&2; exit 1; }

[ "$(grep -c '^  config.json:' "$manifest")" = 1 ] || fail "no single config.json"
[ "$(grep -c '"sharedFileSystemPath": "@VOLUMES_BASE@"' "$manifest")" = 1 ] || fail "the placeholder is not in the shared filesystem setting exactly once"

# The entrypoint substitutes it with this very expression.
expr=$(grep -o 'sed "s|@VOLUMES_BASE@|[^"]*|g"' "$entrypoint") || fail "the entrypoint does not substitute the placeholder"
SKIFF_VOLUMES_BASE="/root/.skiff/volumes/4711-0123abcd"
substituted=$(eval "$expr" < "$manifest")
[[ "$substituted" != *@* ]] || fail "a placeholder is left: $(echo "$substituted" | grep '@')"

python3 - "$substituted" <<'PY'
import json, re, sys
text = sys.argv[1]
m = re.search(r"  config\.json: \|-\n((?:    .*\n)+)", text + "\n")
cfg = json.loads("\n".join(l[4:] for l in m.group(1).splitlines()))
assert cfg["sharedFileSystemPath"] == "/root/.skiff/volumes/4711-0123abcd", cfg
assert cfg["nodePathMap"] == [], "nodePathMap must be empty with a shared filesystem"
docs = [d for d in text.split("\n---\n") if d.strip()]
classes = {}
for d in docs:
    if "kind: StorageClass" in d:
        name = re.search(r"^  name: (\S+)", d, re.M).group(1)
        classes[name] = d
assert set(classes) == {"local-path"}, classes.keys()
for name, d in classes.items():
    assert "volumeBindingMode: Immediate" in d, name
    assert 'defaultVolumeType: "hostPath"' in d, name + ": a local volume needs node affinity, a shared directory has none"
assert "reclaimPolicy: Delete" in classes["local-path"] and "storageclass.kubernetes.io/is-default-class: \"true\"" in classes["local-path"]
assert text.count("rancher/local-path-provisioner:v0.0.32") == 1 and text.count("rancher/mirrored-library-busybox:1.37.0") == 1
PY
echo "storage-manifest-test: ok"
