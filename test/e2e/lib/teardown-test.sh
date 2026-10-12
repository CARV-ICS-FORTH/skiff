#!/usr/bin/env bash
# The deferred teardown that case 01 writes: it cancels only the suite's own job, inspects the
# nodes of each job it cancels, and an empty node list is a failure, not a clean node.
set -euo pipefail
lib_dir=$(cd "$(dirname "$0")" && pwd)
case_dir=$(cd "$lib_dir/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The script writer, without the rest of case 01.
eval "$(awk '/^write_teardown_script\(\) \{/ {on = 1} on {print} on && /chmod \+x/ {print "}"; exit}' "$case_dir/case-01-startup.sh")"

mkdir -p "$tmp/bin"
for tool in scancel sbatch; do
    printf '#!/usr/bin/env bash\necho "%s $*" >> "%s/calls"\necho 99\n' "$tool" "$tmp" > "$tmp/bin/$tool"
done
cat > "$tmp/bin/scontrol" <<SCONTROL
#!/usr/bin/env bash
case "\$*" in
    "show job "*) echo "JobId=\$3 NodeList=\${FAKE_NODELIST:-}" ;;
    "show hostnames "*) [ -z "\$3" ] || echo "\$3" ;;
esac
SCONTROL
chmod +x "$tmp/bin/"*

export E2E_NODES=2
write() { E2E_LOGDIR="$tmp/logs" SKIFF_JOB_ID="$1" write_teardown_script; mkdir -p "$tmp/logs/lib"; cp "$lib_dir"/*.sh "$tmp/logs/lib/"; }
config_script=$(cd "$case_dir/../.." && pwd)/scripts/skiff.sh
run() { PATH="$tmp/bin:$PATH" E2E_CONFIG_SCRIPT="$config_script" E2E_LOGDIR="$tmp/logs" FAKE_NODELIST="${1:-}" SKIFF_JOB_ID="${2:-}" bash "$tmp/logs/case-01-teardown.sh" >"$tmp/run.log" 2>&1; }

# Without the suite's job ID nothing is cancelled.
rm -f "$tmp/calls"; write ""
if run "" ""; then cat "$tmp/run.log"; echo "a teardown without a job ID succeeded" >&2; exit 1; fi
[ ! -e "$tmp/calls" ] || { cat "$tmp/calls"; echo "a teardown without a job ID cancelled something" >&2; exit 1; }

# A job whose nodes cannot be named is not "clean".
rm -f "$tmp/calls"; write 42
if run "" 42; then cat "$tmp/run.log"; echo "an empty node list passed as clean" >&2; exit 1; fi
grep -q 'scancel 42' "$tmp/calls" || { echo "the suite's job was not the one cancelled" >&2; exit 1; }
grep -q 'no node to inspect' "$tmp/run.log" || { cat "$tmp/run.log"; echo "the empty node list was not reported" >&2; exit 1; }
