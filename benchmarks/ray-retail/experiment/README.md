# Ten-node CPU experiment

Five native Slurm and five Skiff measurements, in alternating pairs. Every run
and pilot gets **exactly ten exclusive CPU nodes** in a fresh allocation. The
runner submits one job at a time and confirms release, including COMPLETING,
before submitting another. No command submits jobs unless `campaign.py run`
is explicitly invoked.

The selected backend is now the unmodified official `carvicsforth/skiff:0.1.0`
amd64 image at manifest digest
`sha256:0a17367000907d6957a6f5f8ceb75004ff615423f69350ea884ddeffbb4f4caa`,
with its matching `chazapis` release launcher at commit
`04fbed9c261386a291061f59e4b2b2cd6ceb7162`. This branch supplies the benchmark;
its parallel-startup launcher/runtime are excluded from the primary experiment.
Changing only the image does not select the official startup behavior.

The official source tree supplied as `--skiff-root` contains
`benchmark-runtime.json`: the official image reference/digest, source commit,
launcher checksum, SIF checksum and `kind: official`. Preparation verifies these
checksums and freezes this provenance. The image is converted to SIF without
changing its contents. The current benchmark application SIF is unchanged.

Official Skiff has the `skiff_launch_checked` hook and no fork-specific internal
timing journals. The benchmark wrapper implements that hook, retains common
host/adapter/driver events and independently checks each bubble's named instance is absent **after the
supervisor final sweep**, then checks that credentials were removed. The
earlier post-payload observation is diagnostic: the direct child can exit
while descendants are still draining. Internal entrypoint
subphase timings are unavailable on this backend; they are not fabricated.
Older fork evidence still requires its original timing journals. RDMA and all
common resource, workload, coverage, cleanup and allocation-release gates remain.

## Graceful stop

Use the procedure in [Stopping the cluster](../../../GUIDE.md#stopping-the-cluster):

```sh
scancel --batch --signal=TERM JOB_ID
```

The batch task is the allocation driver. For normal benchmark completion it
requests this same batch-only signal and relays it once to its owned Skiff
supervisor. Launch that supervisor in a separate session: MeluXina delivers
the batch signal to the batch process group. Without isolation, its `srun`
clients receive TERM too and abort their steps before orderly draining. The
supervisor signals the bubble steps and waits for pod/instance
cleanup. Never use plain `scancel`, or send TERM to the `srun` processes, for
normal shutdown.

Slurm delivers the bubble-step signal to both the collector and the bubble
shell. The collector records it without forwarding a duplicate signal: a
second TERM can interrupt the shell's drain handler. Native collectors retain
their payload-forwarding behavior. Repeated batch signals do not interrupt an
ongoing stop. The driver records a failed stop request, supervisor failure or
forced timeout as a failed attempt.

After the official supervisor's final sweep, check every host even if an earlier
check fails. Retain each instance-list command's result and the supervisor's
credential-removal observation. These records diagnose failures; they do not
relax the requirement for successful cleanup on every host. Modified adapters
require a fresh immutable source/campaign; never patch an active or failed
campaign in place.

The opt-in `tests/slurm_stop_probe.py` and `tests/slurm_stop_probe.slurm` compare
inherited and isolated supervisor groups inside one ten-node allocation. Stage
both together, set `PROBE_ROOT` to that shared directory, and submit the Slurm
file explicitly. Its witness workers perform no benchmark work. Keep the probe
log, accounting and charge as diagnostics, never as a measurement. Live probe
5454378 reproduced the inherited-group failure and verified isolation in nine
seconds on ten nodes.

An explicit user acceptance of historical cleanup evidence can be recorded as
a run-specific `cleanup_exception` with `authorized_by: user`, the matching
Slurm `job_id`, `scope: official_cleanup_only`, and a reason. This applies only
to the official per-host/supervisor cleanup checks for that job. All other
validation, transport, accounting and release requirements remain enforced.
The resulting status is `ACCEPTED_WITH_CLEANUP_EXCEPTION`, and the exception
is retained in the final campaign summary. It is not inherited by later runs.
Cleanup timing values from an exception run must be labelled as observations
of a defective stop, not evidence of successful graceful shutdown. Keep the
original failed campaign and its evidence unchanged; store acceptance reports
in the replacement campaign.

## What is prepared

The measurement and pilot profiles now use the **same bounded workload**:
40,000 series × 1,000 days; four training epochs capped at 128 batches;
40 concurrent Tune trials with two capped epochs; eight PPO iterations;
20,000 serving requests. Both have a 45-minute application watchdog. The user
selected five real workload pairs with observed CPU use on 2026-11-13;
sustained saturation is no longer an acceptance requirement. Ten exclusive
nodes and the full resource budgets remain mandatory. Qualification still
requires correct artifacts, equivalent deployments, transport, cleanup and
release evidence. Pilots remain excluded from measurements.
The historical CPU and GPU profiles keep their original budgets and node counts.

Both adapters consume one `deployment` contract:

| Per-node setting | Both backends |
| --- | --- |
| Allocation | Exclusive CPU node, 256 hardware threads, all Slurm memory |
| Workload affinity | One allowed SMT sibling from each of 128 physical cores |
| Ray CPU scheduling tokens | 128 |
| Training | Four ranks × 31 CPUs/threads = 124, leaving room for coordination/data |
| Object store | 64 GiB, verified tmpfs backing and sufficient free capacity |
| Ray schedulable heap | 384 GiB |
| Pod scheduling memory envelope | 472 GiB |
| Minimum physical RAM | 480 GiB; effective cgroups must admit the application envelope |

Native object storage uses host tmpfs. The Skiff experiment uses a pod-owned
`emptyDir` with `medium: Memory` and a 65 GiB size limit for the 64 GiB Ray budget.
The staged Skifflet implements this as an actual tmpfs; workload startup still
checks its backing filesystem and free capacity. A `/dev/shm` hostPath is outside
Skiff's allowed shared-state/scratch roots and is rejected by capability checks.

The 448 GiB combined Ray budgets leave 24 GiB in the application envelope and
8 GiB of the schedulable node RAM outside it. Ray's heap budget is a scheduling
budget, not preallocation or a hard RSS limit. Full usable memory is accessible;
the workload does not fill RAM artificially. Affinity constrains the workload,
not Skiff's system daemons. Platform use is counted in host measurements.

Data generation has 2,560 blocks and up to 1,280 pending tasks; Tune has 40
concurrent trials; PPO has 1,100 environment runners; batch evaluation has 150
eight-CPU actors. Serving retains one replica of each model per node, with eight Torch threads
per replica matching its CPU reservation. Some
phases inherently use fewer cores. CPU reservations and thread counts do not
establish saturation. This campaign reports actual per-host and per-phase use;
it does not claim full-core saturation.

The original 1,200-runner candidate stalled in the native pilot: its 80 free CPU
tokens were fragmented across ten nodes, preventing placement of the 16-CPU
learner. Both profiles now use 1,100 runners, retaining the same learner budget,
training batches and iteration counts. Preflight rejects layouts that can leave
every node below the learner's CPU request, allowing one coordination CPU.

Native node addresses and Gloo select `ib0`. Skiff selects the same physical
uplink and retains `SKIFF_RDMA=auto`. Actual transport, peer state, failures and
fallback are retained from Plaid, alongside IB counters and socket/route/MTU
evidence. Both execute an identical bounded 8 MiB-per-node Ray transfer probe
before the application. It includes scheduling/serialization and is not a
standalone fabric-bandwidth benchmark. The platform comparison includes the
network implementation; it cannot isolate orchestration overhead.

## Before submission

Stage an isolated copy of the benchmark under the host's `~/.skiff` shared-state
tree, using shared project storage (a project-backed `.skiff` symlink is fine).
The same tree must contain the CPU SIF and campaign output. This is required
because Skiff exposes that tree at `/root/.skiff` inside its bubble. Do not point
the experiment at a source directory outside this mount or overwrite old results.

Provide the qualified Skiff source/release tree and matching, RDMA-capable runtime
at `~/.skiff/images/skiff.sif`, with its host tools and system-pod image cache
already prepared. Do not build/download images in timed allocations. Existing
Skiff clusters/startup ownership cause the experiment to refuse startup. The
current production `skiff.sh` cluster implementation is reused unchanged, with
an experiment-specific Slurm launcher to wrap each bubble in host collection.

The host Python needs Python 3.6+ and only its standard library. Application and
pod rendering use the pinned Python 3.11 CPU SIF. `sbatch`, `squeue`, `sacct`,
`scontrol`, `kubectl`, `apptainer` and network tools must be available in the
batch environment. `chronyc tracking` is retained when accessible; unavailable
clock synchronization evidence must be disclosed before cross-host timestamp
interpretation. Refresh `myquota -t compute --time minutes` and verify shared
storage quotas/capacity before submission.

From the staged benchmark directory, preparation is read-only towards Slurm:

```sh
python3 experiment/campaign.py prepare "$HOME/.skiff/experiments/ray-ten-node-CAMPAIGN" \
  --image "$HOME/.skiff/images/ray-retail-cpu.sif" \
  --runtime-image "$HOME/.skiff/images/skiff.sif" \
  --skiff-root /shared/qualified-skiff \
  --host-python /usr/bin/python3 \
  --max-node-hours 240
```

Preparation records source/deployment/configuration/application-image/runtime
identities and the fixed sequence, and freezes explicit network/MTU/runtime-path
settings for subsequent jobs. It does not stage files, mutate existing images,
start a cluster or submit jobs. Configuration or code changes after preparation
require a new campaign. The submission and allocation both recheck identities.

Only when ready to spend credits, explicitly run:

```sh
python3 experiment/campaign.py run "$HOME/.skiff/experiments/ray-ten-node-CAMPAIGN"
```

Initially this submits only the two pilots, sequentially. It stops on failed
validation and stops before measurements until qualification. Review their raw
reports, including actual network transport, collector overhead, clock evidence,
and sustained/per-phase utilization. Qualification requires increasing RDMA byte
counters without transport-error/fallback evidence on every Skiff host. Native
training must record Gloo on ib0. Measurements with transport deviations stop the campaign and remain excluded.
When a utilization floor is selected, falling below it also stops the campaign. A pilot is not silently counted as a
measurement. If sizing/placement/transport changes, requalify a new campaign.

For this campaign, record the user-selected utilization policy:

```sh
python3 experiment/campaign.py qualify "$HOME/.skiff/experiments/ray-ten-node-CAMPAIGN" \
  --observe-utilization
```

This requires identical pilot and measurement profiles. CPU use is reported
without claiming saturation. `--minimum-busy-percent N` remains available for
campaigns that explicitly require a utilization floor.

This revalidates both pilots and their workload equivalence and records the
policy; it does not submit. A subsequent explicit `run` executes the ten
measurements sequentially. Failed attempts and charges remain in campaign state;
there are no automatic retries. An interrupted runner retains the active job
and resumes observation rather than submitting another. An uncertain submission
result requires scheduler reconciliation and never triggers blind resubmission.

Each job has a two-hour scheduler ceiling; the selected application and pilot
profiles both have a 45-minute watchdog. The campaign reserves 20 node-hours
against its ceiling before each submission. **240 node-hours is the conservative
maximum envelope for twelve two-hour allocations**, not an expected cost. The
earlier 120-node-hour figure assumed one hour each. Actual successful and failed
allocation charges are retained, and the runner stops if another job's maximum
charge would exceed the configured campaign ceiling.

## Evidence and interpretation

Each run has:

- `application/manifest.json` and `resources.json`: all eight phases, workload
  identity/quality, artifact hashes, CPU/memory/network samples, Ray scheduling
  availability and object-store/spill/restore summary (every ten seconds).
- `operations/hosts/HOST/`: initial topology/cgroup/route/clock probe, two-second
  CPU/memory/disk/network/IB counters, job-scoped PID/start-time CPU/RSS/I/O and
  ten-second PSS/socket/Plaid snapshots, with collection cost and missing data.
- `operations/adapters/`: same Ray launch/readiness/transfer/application/stop
  events on both backends, including forced-kill/remaining-process evidence.
- `operations/driver/`: backend launch, apply, readiness and shutdown markers.
  Skiff also retains pod placement, events, head logs and production lifecycle
  timing files including per-node cleanup.
- `observer.json`: final scheduler state/exit/allocation duration, and separately
  the last active and first absent observations. Accounting End alone does not
  prove release. Queue/release observation has two-second polling granularity
  plus scheduler-command latency; a disconnected observer can add uncertainty.
- `validation.json`: resource/placement/identity/coverage/cleanup checks, common
  timing metrics, per-phase CPU samples, and sampled process-category overhead.

Collectors start immediately before the native container or Skiff bubble launch,
and stop after that payload finishes cleanup. Driver events cover earlier
allocation setup and later supervisor cleanup; host samples do not cover the
small intervals before/after their wrapper. PID ownership uses exact Slurm
cgroup/job identity or observed descendants, not all the user's processes.
Sampled process CPU deltas miss lifetimes between samples; cgroup counters and
host totals are retained independently. RSS sums can double-count shared pages;
PSS and job-level memory counters help interpretation. Native-only absent Skiff
fields are not fabricated zeros. Actor/replica placement and task counts remain
application-specific evidence alongside host totals.

Use allocation-to-application, application duration and application-end-to-backend-
stopped as common intervals. Ray startup/teardown and backend-specific cleanup
are separate subintervals; Skiff's shutdown removes a cluster after Ray/pods have
stopped, whereas native cleanup mostly consists of Ray/container exit. Do not
interpret a sum of overlapping lifecycle intervals as total overhead. Skiff
per-node entrypoint and bubble cleanup plus supervisor cleanup must all exist.
The resource comparator retains strict historical tmpfs checks by default; this
experiment explicitly compares matched usable object-store budgets on tmpfs,
allowing harmless differences in whole-host tmpfs total capacity.
Effective memory caps, CPU bounds, CPU model/microcode, kernel and NUMA exposure
must also match between compared backends; equal declared Ray budgets alone
cannot qualify a pair.

At completion `summary.json` contains five values per backend, means, sample SD,
CV, medians/ranges, Student-t 95% intervals, phase summaries, paired ratios and
differences, per-run quality/serving results and total node-hour charges. A mean
of per-run p99 latency is not a pooled request-level p99. Five pairs quantify
initial variation but cannot establish causation for small differences.
