#!/usr/bin/env bash
# The pipefail guard: fail on a pipeline whose last command stops reading early, in a
# script that runs under pipefail.
#
#   test/pipefail-guard.sh [FILE...]    (no files: every shell script of the repository)
#
# `cmd | grep -q X`, `cmd | head -1`, `cmd | awk '... exit ...'` and `cmd | sed -n '...q'`
# end when they have what they want, while `cmd` may still have output to write: it dies
# of SIGPIPE, and under pipefail the whole pipeline fails although the answer was there.
# It made the supervisor report a living bubble as dead (and end the job) whenever the
# user had other Apptainer instances on the node. shellcheck does not report it.
#
# Read the whole output first (skiff_listed, e2e_grep_q, a variable and [[ ]]), or use a
# reader that reads to the end (`sed -n 1p`, `awk 'NR == 1'`, `pgrep -o`). A line that is
# safe for a reason that is written next to it carries `pipefail-ok` in a comment.
set -u
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_dir" || exit 1

if [ "$#" -gt 0 ]; then
    files=("$@")
else
    files=()
    while IFS= read -r f; do files+=("$f"); done < <(find scripts images test -type f \( -name '*.sh' -o -name '*.slurm' \) -not -path '*/results/*' -not -path '*/.*' | sort)
fi

early='\|[[:space:]]*(grep[[:space:]]+(-[A-Za-z]*q[A-Za-z]*|--quiet|-m[[:space:]]*[0-9]+|--max-count)|head([[:space:]]|$)|awk[[:space:]]+.[^|]*[^A-Za-z_]exit[^A-Za-z_]|sed[[:space:]]+(-n[[:space:]]+)?.[^|]*q.([[:space:]]|$))'
status=0
for f in "${files[@]}"; do
    grep -q pipefail "$f" || continue
    while IFS= read -r hit; do
        text=${hit#*:}
        [[ "$text" =~ ^[[:space:]]*# ]] && continue
        [[ "$text" == *pipefail-ok* ]] && continue
        echo "$f:$hit" >&2
        status=1
    done < <(grep -n -E "$early" "$f" || true)
done
if [ "$status" -ne 0 ]; then
    echo "pipefail-guard: the lines above read only part of a pipe under pipefail (see test/pipefail-guard.sh)" >&2
fi
exit "$status"
