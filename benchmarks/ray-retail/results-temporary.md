# Ray Retail — temporary five-pair results

Collected: 2026-11-13T11:27:30.954932+00:00.

**Temporary report, not the final five-pair aggregate.** Pair 1 is retained
following the user's explicit acceptance of Skiff's cleanup deviation. Native
passed strict validation. Skiff passed every other gate, including actual RDMA
on all ten hosts, and is labelled `ACCEPTED_WITH_CLEANUP_EXCEPTION`.
There are **1/5 accepted complete pairs**. The original cleanup failure remains
in the original campaign and evidence snapshot; it has not been relabelled PASS.

Keep this report separate from [results.md](../results.md). After five accepted
pairs are collected, publish the cohort statistics there with the cleanup
exception disclosed. Ordinary failed attempts remain supporting evidence.

## Pair 1 status

| Field | Native Slurm | Official Skiff |
| --- | --- | --- |
| Job ID | 5453941 | 5454052 |
| Role | Fresh measurement, pair 1 | Fresh measurement, pair 1 |
| Nodes | 10 exclusive CPU nodes | 10 exclusive CPU nodes |
| Slurm result | COMPLETED, 0:0 | COMPLETED, 0:0 |
| Application | Complete, all eight phases | Complete, all eight phases |
| Evidence validation | PASS | **ACCEPTED_WITH_CLEANUP_EXCEPTION**; other checks PASS |
| Final-cohort eligibility | Accepted native run | Accepted by explicit user exception |
| Allocation duration | 1278 s | 1283 s |
| Charged node-hours | 3.550000 | 3.563889 |
| Allocation release observed | Yes | Yes |

These two jobs cost 7.113889 node-hours combined, excluding pilots and previous attempts.

## Observed times

All values below are seconds. Skiff/native is the observed duration ratio.
One accepted pair cannot establish variability or a confidence interval. The
Skiff cleanup defect is retained as an explicit exception; cleanup timings from
that run do not demonstrate a successful graceful stop.

| Metric | Native | Skiff (cleanup exception) | Skiff/native |
| --- | ---: | ---: | ---: |
| Application lifecycle window | 1252.452 | 1195.890 | 0.9548 |
| Ray startup | 7.486 | 6.297 | 0.8412 |
| data | 569.018 | 562.551 | 0.9886 |
| tune | 36.037 | 35.332 | 0.9804 |
| train | 193.069 | 180.225 | 0.9335 |
| batch | 47.992 | 41.001 | 0.8543 |
| policy | 182.314 | 172.853 | 0.9481 |
| evaluation | 12.740 | 12.519 | 0.9826 |
| serve | 198.533 | 181.272 | 0.9131 |
| recovery | 3.496 | 3.218 | 0.9206 |

The application window comes from the head adapter’s application start/finish
events; phase times come from the manifest. It includes small gaps between phases.
Allocation duration is scheduler accounting and includes setup/teardown.

Validated application-window mean physical-core busy time: **30.88% native /
31.83% Skiff**. The frozen policy reports observed utilization without requiring
saturation. Skiff's non-cleanup resource, pod, sampling, accounting and transport
checks passed on revalidation.

## Application quality and serving

Application-only artifact/resource comparison **PASS** using the experiment-budget
comparator, including artifact hashes and synthetic dataset checksums. This does
not certify cleanup; that deviation was separately accepted by the user. Numerical model outputs can vary between
independent distributed runs.

| Metric | Native | Skiff (cleanup exception) |
| --- | ---: | ---: |
| Training validation MAE | 5.235709 | 5.239239 |
| Batch test rows | 6,000,000 | 6,000,000 |
| Batch test MAE | 4.998346 | 4.995572 |
| Batch test WAPE | 0.132790 | 0.132716 |
| Seasonal baseline MAE | 9.696431 | 9.696431 |
| Serving requests | 20,000 | 20,000 |
| Serving errors | 0 | 0 |
| HTTP load window (s) | 183.560 | 168.679 |
| Serving requests/s | 108.956 | 118.569 |
| Serving p50 latency (ms) | 1615.149 | 1478.163 |
| Serving p95 latency (ms) | 5270.484 | 4902.019 |
| Serving p99 latency (ms) | 6262.506 | 5500.364 |

Both runs verified direct-checkpoint inference equivalence, observed ten forecast
and ten policy replicas, and recorded zero serving errors. Serving latency
percentiles describe each run’s own requests; they are not pooled cohort values.
Both recovery phases recorded one intentional actor failure and matching
restored/duplicate ledger totals of 18.

| Inventory policy (1,200 episodes each) | Native mean cost | Skiff mean cost | Native fill rate | Skiff fill rate |
| --- | ---: | ---: | ---: | ---: |
| ppo | 786.951 | 796.726 | 99.175% | 99.218% |
| forecast_base_stock | 3412.348 | 3422.112 | 83.490% | 83.338% |
| reorder_point | 3892.346 | 3892.346 | 79.882% | 79.882% |

## Recorded cleanup deviation and explicit acceptance

Original campaign failure: `Missing/failed official bubble cleanup verification: mel0523`.

The retained journals contain one final cleanup verification, on `mel0047`,
with `instance_absent: false` and payload status `-15`. Final verification
events are absent on the other nine hosts, and the supervisor-cleanup-finished
event is absent. The end hook returns on the first failed host check, which
explains why the validator can report a different missing host (`mel0523`).

`instance_absent: false` can indicate a remaining instance or a failed/unparseable
instance-list query. The saved event does not distinguish those cases; it is
insufficient to assert a confirmed instance leak. Slurm completion and observed
release do not replace the required per-host cleanup evidence.

The original r9 campaign stopped before pair 2 and is unchanged. The user
subsequently requested retaining pair 1 and continuing with pair 2. That
acceptance is scoped to Skiff job **5454052**, official cleanup checks only;
all other gates passed a fresh revalidation. See the
[acceptance and validation snapshot](../evidence/meluxina/ray-retail-pair-1-acceptance.json).
The recorded error can name different missing hosts because host validation
order is not fixed; the original failure message is preserved above.

The current replacement campaign is `campaign-20261113-r12-pairs`, using immutable
`source-r12-isolated-stop`. It carries both pilots and pair 1 with original job IDs and
charges. Pair 2 retries Skiff as **job 5454387, RUNNING on ten nodes** at the
continuation check, then native. Previous attempt 5454239 failed during shutdown
and is excluded, as detailed below. The runner continues pairs 3–5 sequentially
and retains strict cleanup validation for every future run.

The benchmark-only graceful-stop fix uses the documented batch-only TERM and
prevents duplicate forwarding by bubble collectors. All **67 tests pass**;
the first live pair-2 attempt exposed the additional inherited-process-group
issue. A separate supervisor session is now verified by the live signal probe;
full Skiff cleanup validation awaits the retry. The official runtime/launcher and workload
remain unchanged. Final summaries retain the run-specific acceptance exception.

## Frozen workload and provenance

- 40,000 series × 1,000 days, 2,560 data blocks; 6,000,000 test rows.
- 40 training ranks, 31 threads/CPUs per rank; four epochs, at most 128 batches per epoch.
- 40 concurrent Tune trials, two epochs with the same training batch cap.
- 1,100 PPO environment runners, a 16-CPU learner, eight iterations and 65,536-step batches.
- 20,000 HTTP requests at concurrency 256; ten replicas per model, eight threads per replica.
- Per node: 128 Ray CPU tokens, 64 GiB object store, 384 GiB Ray heap, 472 GiB pod memory envelope.
- Same bounded pilot/measurement profiles, seed 731 and 45-minute application watchdog.
- Application: Ray 2.49.2 / Torch 2.8.0+cpu; official unmodified Skiff 0.1.0 runtime/launcher.
- Verified transport: native Gloo/TCP on IPoIB; Skiff RDMA on all ten hosts, with no transport deviations.
- Application source digest: `3c803d277297badbe26c60cd25a4e5533ddb0aafd2791d0b2c7693d7af9a7c1a`.
- Resolved configuration digest: `3c1c8c897c88360e41591962c45d3efaadb7043468fd0fc6ab4b338463a98cb6`.

Campaign: `/home/users/u102006/.skiff/experiments/campaign-20261113-r9-pairs`.

Raw remote run directories:

- Native: `/home/users/u102006/.skiff/experiments/campaign-20261113-r9-pairs/pair-1-slurm`.
- Skiff: `/home/users/u102006/.skiff/experiments/campaign-20261113-r9-pairs/pair-1-skiff`.

The compact [evidence snapshot](../evidence/meluxina/ray-retail-pair-1-provisional.json)
preserves accounting, configuration/identity, phase/quality/serving metrics,
comparison output and cleanup records. Full manifests, artifacts, host samples
and backend logs remain in the remote campaign directories.

## Remaining pairs

| Pair | Native | Skiff | Accepted complete pair |
| --- | --- | --- | --- |
| 1 | PASS: 5453941 | ACCEPTED_WITH_CLEANUP_EXCEPTION: 5454052 | Yes, exception disclosed |
| 2 | Not submitted | Retry RUNNING: 5454387; 5454239 excluded | Pending |
| 3 | Not submitted | Not submitted | No |
| 4 | Not submitted | Not submitted | No |
| 5 | Not submitted | Not submitted | No |

Acceptance recorded: 2026-11-13T11:43:17.197356+00:00. Current state is a snapshot; query the campaign before acting.

## Pair-2 shutdown failure and correction

Skiff 5454239 completed all eight application phases but failed allocation
validation with exit 1:0. The batch-only TERM also reached inherited `srun`
clients; they aborted the steps, preventing collector exit journals. The final
sweep confirmed all ten instances absent and credentials removed, but this does
not establish graceful cleanup. This attempt is **excluded**, with no extension
of the user-authorized pair-1 exception. Native was not submitted because the
runner correctly stopped at the failed validation gate.

| Excluded Skiff 5454239 phase | Observed seconds |
| --- | ---: |
| data | 565.036 |
| tune | 35.523 |
| train | 184.722 |
| batch | 42.334 |
| policy | 176.315 |
| evaluation | 12.904 |
| serve | 188.005 |
| recovery | 3.199 |

The correction launches the Skiff supervisor in its own session and relays
TERM only to its PID, protecting its `srun` clients from the batch-group signal.
Actual ten-node probe 5454378 reproduced inherited client exit 137, while the
isolated client remained alive until its worker received the ordered step TERM
and then exited 0. Probe allocation: nine seconds, 0.025 node-hours. The excluded
benchmark attempt cost 3.688889 node-hours. All 67 local regressions pass.

The [failure and probe snapshot](../evidence/meluxina/ray-retail-pair-2-stop-failure.json)
retains driver events, failed accounting, final instance checks and actual probe
results. Remote originals remain unchanged. Retry 5454387 will need strict
application, transport, cleanup and accounting validation before native follows.
No pair-2 result is included in the accepted cohort yet.
