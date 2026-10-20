#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
benchmark_dir=$(cd -- "$script_dir/.." && pwd)
cd "$benchmark_dir"
export KUBECONFIG=${KUBECONFIG:-$HOME/.skiff/kubeconfig}
export PATH="$HOME/.skiff/tools/bin:$PATH"

gpu=false
for argument in "$@"; do
    if [[ $argument == --gpu ]]; then gpu=true; fi
done
if [[ $gpu == true ]]; then
    image_host=${BERT_SIF:-$HOME/.skiff/images/bert-training-gpu.sif}
    image_guest=${BERT_IMAGE:-/root/.skiff/images/bert-training-gpu.sif}
else
    image_host=${BERT_SIF:-$HOME/.skiff/images/bert-training-cpu.sif}
    image_guest=${BERT_IMAGE:-/root/.skiff/images/bert-training-cpu.sif}
fi
test -s "$image_host"
image_sha256=$(sha256sum "$image_host")
image_sha256=${image_sha256%% *}

namespace=${BERT_NAMESPACE:-bert-benchmark}
run_id=${BERT_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ | tr '[:upper:]' '[:lower:]')}
name=${BERT_JOB_NAME:-bert-$run_id}
storage=${BERT_STORAGE:-/root/.skiff/benchmarks/bert-distr-training}
run_dir=${BERT_RUN_DIR:-results/$run_id}
manifest_dir="$benchmark_dir/manifests"
manifest="$manifest_dir/$name.json"
mkdir -p "$manifest_dir" "$benchmark_dir/$run_dir"

python3 "$benchmark_dir/run.py" render \
    --name "$name" --namespace "$namespace" \
    --image "$image_guest" --image-sha256 "$image_sha256" \
    --storage-kind hostPath --storage "$storage" \
    --run-dir "$run_dir" --output "$manifest" "$@"
kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$manifest"
echo "Submitted $namespace/$name; results: $benchmark_dir/$run_dir"

while true; do
    succeeded=$(kubectl get pytorchjob "$name" -n "$namespace" \
        -o jsonpath='{range .status.conditions[?(@.type=="Succeeded")]}{.status}{end}' 2>/dev/null || true)
    failed=$(kubectl get pytorchjob "$name" -n "$namespace" \
        -o jsonpath='{range .status.conditions[?(@.type=="Failed")]}{.status}{end}' 2>/dev/null || true)
    if [[ $succeeded == True ]]; then break; fi
    if [[ $failed == True ]]; then
        kubectl get pods -n "$namespace" -l "bert-run=$name" -o wide >&2 || true
        exit 1
    fi
    sleep 10
done
python3 "$benchmark_dir/validate_result.py" "$benchmark_dir/$run_dir/result.json"
