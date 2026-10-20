# Benchmark results

Successful runs reviewed through 2026-10-19, retained once per benchmark,
execution platform and site. The existing Knot records were reviewed on
2026-10-15; the ten-node MeluXina Spark runs were collected on 2026-10-19.
These are individual measured runs, not averages across independent
submissions. Failed attempts, setup checks and superseded runs are excluded
from the result tables.

## Successful runs

| Benchmark | Site | Platform | Date | Nodes | Run | Workload wall time |
| --- | --- | --- | --- | ---: | --- | ---: |
| Micro Spark | Knot | Skiff / Kubernetes | 2026-10-07 | 3 | `20261007t111827-6a2be420z` | 36.683 s |
| Spark Knot: generation | Knot | Skiff / Kubernetes | 2026-10-07 | 3 | `20261007t144200-c1c1f3a9z` | 231.966 s |
| Spark Knot: queries | Knot | Skiff / Kubernetes | 2026-10-07 | 3 | same run, separate application | 201.500 s |
| Dask matrix | Knot | Skiff / Kubernetes | see execution record | 3 | controlled three-worker rerun, Slurm allocation 103 | 36.137 s |
| Spark Knot: generation | Knot | Native Slurm / Apptainer | 2026-10-14 | 2 | job 710, `710.Ojpk9x` | 225.441 s |
| Spark Knot: queries | Knot | Native Slurm / Apptainer | 2026-10-14 | 2 | same job, separate application | 214.306 s |
| Dask matrix | Knot | Native Slurm / Apptainer | 2026-10-13 | 2 | job 635, `635.ihEZ6O` | 21.948 s |
| Spark Knot: generation | MeluXina | Skiff / Kubernetes | 2026-10-19 | 10 | job 5357975, `meluxina-5357975` | 92.908 s |
| Spark Knot: queries | MeluXina | Skiff / Kubernetes | 2026-10-19 | 10 | same job, separate application | 98.775 s |
| Spark Knot: generation | MeluXina | Native Slurm / Apptainer | 2026-10-19 | 10 | job 5358076, `5358076.8IZPUC` | 141.639 s |
| Spark Knot: queries | MeluXina | Native Slurm / Apptainer | 2026-10-19 | 10 | same job, separate application | 117.940 s |

The supplied Micro Spark, Spark Knot and three-worker Dask numbers match the
retained successful records; they are included once, not counted as new runs.
The three-worker Dask record does not preserve an exact execution timestamp,
so none is inferred from the date of its Git commit.

## Micro Spark

Historical results are retained here after removal of the smaller benchmark.
The maintained Spark workload is now [analytics-spark](analytics-spark/README.md).

Spark 3.5.3, three one-core executors with 512 MiB heap each, one on each of
`k8s-node1`, `k8s-node2`, and `k8s-node3`. The workload used 100,000 records,
three partitions and three query repetitions, plus RDD, SQL, caching and
MLlib K-means checks. Structured status was `PASS`; the application completed
in one attempt. All three executors participated, with 19, 22 and 48 completed
tasks and zero failed tasks. Driver and executors had zero restarts.

| Metric | Value |
| --- | ---: |
| Workload wall time | 36.683 s |
| Aggregate executor task / CPU time | 25.027 s / 17.990 s |
| Task CPU utilization | 71.9% |
| Aggregate JVM GC time | 0.225 s |
| Shuffle remote read / total read / written | 2.00 MiB / 3.02 MiB / 3.14 MiB |
| Shuffle fetch wait / write time | 0.000 s / 0.103 s |
| Memory / disk spill | 0 B / 0 B |
| Completed stages | 45 |

Source: [selected original metrics](evidence/knot-platform/micro-spark-skiff.json).

## Spark Knot analytics on Knot

Both platforms used Spark 4.0.4, TPC-DS scale factor 1, all 24 tables,
24 generation and shuffle partitions, four cores and 8 GiB heap per executor,
and a four-core driver with 4 GiB heap. The original query selection was
`q1,q10,q11,q12,q13,q14a,q14b,q15,q16,q17,q18,q19,q2,q20,q21,q22`, executed
in that order for three iterations: 48 query executions. Generation and
queries both produced `PASS` records, and all expected executors participated.
The Skiff applications each completed in one attempt; Slurm job 710 completed
with exit `0:0` and launcher exit `0`.

| Metric | Skiff: generate (3 nodes) | Skiff: query (3 nodes) | Slurm: generate (2 nodes) | Slurm: query (2 nodes) |
| --- | ---: | ---: | ---: | ---: |
| Workload wall time | 231.966 s | 201.500 s | 225.441 s | 214.306 s |
| Aggregate executor task time | 345.255 s | 596.376 s | 377.302 s | 572.087 s |
| Aggregate executor CPU time | 286.838 s | 348.289 s | 258.966 s | 328.764 s |
| Task CPU utilization | 83.1% | 58.4% | 68.6% | 57.5% |
| Aggregate JVM GC time | 2.769 s | 2.719 s | 7.375 s | 6.938 s |
| Remote shuffle read | 0 B | 36.63 MiB | 0 B | 79.76 MiB |
| Total shuffle read | 0 B | 689.84 MiB | 0 B | 689.84 MiB |
| Shuffle written | 0 B | 552.79 MiB | 0 B | 552.79 MiB |
| Shuffle fetch wait | 0.000 s | 0.030 s | 0.000 s | 0.033 s |
| Shuffle write time | 0.000 s | 3.017 s | 0.000 s | 2.688 s |
| Data read | 0 B | 1.17 GiB | 0 B | 1.17 GiB |
| Data written | 355.57 MiB | 3.52 KiB | 355.57 MiB | 3.52 KiB |
| Memory / disk spill | 0 B / 0 B | 0 B / 0 B | 0 B / 0 B | 0 B / 0 B |
| Completed stages | 25 | 452 | 25 | 452 |

Native Slurm used `k8s-node1` and `k8s-node3`, with one executor per node,
explicit CPU binding to cores, tmpfs Spark scratch and shared-home MinIO
storage. Its application-step elapsed times were 241 s for generation and
229 s for queries; total allocation runtime was 503 s (8m23s), excluding
queue time. These include startup/shutdown and are distinct from the workload
wall times above. Credentials and temporary MinIO data were removed and
benchmark service ports were closed after completion.

Sources: [Skiff phase summaries](evidence/knot-platform/spark-knot-skiff.json),
[Slurm phase summaries](evidence/knot-platform/spark-knot-slurm.json), and
[Slurm setup and run record](analytics-spark/slurm/README.md#knot-platform-run-2026-10-14).
The selected JSON preserves per-query minimum, median and maximum timings.

## Spark Knot analytics on MeluXina

Slurm job **5357975** completed with exit `0:0` and benchmark exit `0` on
2026-10-19. The allocation ran from **10:12:32 to 10:20:17 UTC**, taking
465 s (7m45s), including cluster startup, installation, both applications and
cleanup. This allocation time is distinct from the workload timings below.

The freshly rebuilt local Skiff was `0.1.0-e1bad50`, commit
`e1bad5010a35bb8bb663c7ff7bd54013decc1217`, with Apptainer 1.5.4.
Ten exclusive CPU nodes each provided 256 logical CPUs and 480 GiB memory.
The workload used one four-core executor per node, each with 8 GiB heap and
2 GiB overhead, and a four-core driver with 4 GiB heap and 1 GiB overhead.
Those executor resources describe 40 configured Spark cores, rather than
use of all 2560 logical CPUs in the allocation.

Spark 4.0.4 / Python 3.10.12 retained scale factor 1, all 24 tables,
24 generation/shuffle partitions and the original 16-query order listed
above, repeated three times (48 executions). Both structured phase results
were `PASS`, and both applications completed in one submission/execution
attempt. All ten executors participated in each phase, completing 616
generation tasks and 3194 query tasks, with **zero failed Spark tasks**.
Both drivers exited `0` with zero restarts.

Native Slurm job **5358076**, run `5358076.8IZPUC`, also completed with exit
`0:0` and launcher exit `0`, from **10:48:12 to 10:53:50 UTC** on 2026-10-19.
Its total allocation time was 338 s (5m38s), excluding queue time. It used
Apptainer and Spark standalone directly, without Skiff or Kubernetes, with
the same Spark SIF, benchmark/query sources, workload and ten four-core
executors. Both phases passed the native result validator: all 24 tables and
48 ordered query executions, all ten executors active and participating,
616 generation / 3194 query tasks completed, and zero failed Spark tasks.
Application-step times were 152 s for generation and 136 s for queries,
including Spark startup/shutdown; workload timings below exclude session startup.

| Metric | Skiff: generate (10 nodes) | Skiff: query (10 nodes) | Slurm: generate (10 nodes) | Slurm: query (10 nodes) |
| --- | ---: | ---: | ---: | ---: |
| Workload wall time | 92.908 s | 98.775 s | 141.639 s | 117.940 s |
| Aggregate executor task time | 200.428 s | 369.415 s | 283.036 s | 488.678 s |
| Aggregate executor CPU time | 136.718 s | 230.956 s | 147.116 s | 221.830 s |
| Task CPU utilization | 68.2% | 62.5% | 52.0% | 45.4% |
| Aggregate JVM GC time | 6.243 s | 4.927 s | 6.329 s | 6.389 s |
| Remote shuffle read | 0 B | 115.08 MiB | 0 B | 115.80 MiB |
| Total shuffle read | 0 B | 689.84 MiB | 0 B | 689.84 MiB |
| Shuffle written | 0 B | 552.79 MiB | 0 B | 552.79 MiB |
| Shuffle fetch wait | 0.000 s | 0.014 s | 0.000 s | 0.142 s |
| Shuffle write time | 0.000 s | 1.151 s | 0.000 s | 1.165 s |
| Data read | 0 B | 1.17 GiB | 0 B | 1.17 GiB |
| Data written | 355.57 MiB | 3.51 KiB | 355.57 MiB | 3.52 KiB |
| Memory / disk spill | 0 B / 0 B | 0 B / 0 B | 0 B / 0 B | 0 B / 0 B |
| Completed stages | 25 | 452 | 25 | 452 |

Skiff placement samples confirm ten distinct physical nodes in both phases:
`mel0066`, `mel0097`, `mel0128`, `mel0202`, `mel0217`, `mel0272`,
`mel0303`, `mel0325`, `mel0375`, and `mel0441`.

Native Slurm used `mel0029`, `mel0039`, `mel0061`, `mel0178`, `mel0312`,
`mel0316`, `mel0373`, `mel0426`, `mel0452`, and `mel0466`. Its role resource
files record actual CPU affinity with `CPU_BIND=cores`; Spark scratch was
tmpfs, while MinIO data was on shared-home **Lustre**. Skiff's MinIO emptyDir
was on node-local tmpfs. Native Spark used `ib0` directly; Skiff used its
virtual network. These differences and single-run sampling prevent attributing
the timing difference to platform overhead or a controlled speedup.

Native workers retried initial master connections before registration, then
completed both workloads. Worker logs show executor termination requested
after each application. The native driver steps completed with exit `0:0`;
long-running service steps record `FAILED`, exit `255:0`, following the
launcher's teardown. Final allocation/launcher exits were zero. Cleanup
inspection confirmed credentials and temporary MinIO data were removed.

The saved Skiff SparkApplication records label all ten executors `FAILED` in each
phase despite application `COMPLETED`, driver exit `0` and zero failed Spark
tasks at metric capture. Executor exit codes/events/logs were not retained,
so the cause is unconfirmed; clean executor shutdown is not established.
Separately, the wrapper sent SIGTERM to the cluster after
`BENCHMARK_SUCCEEDED`; cancelled long-running Slurm cluster steps belong to
that teardown. The cluster log confirms cleanup completed. Final allocation
status comes from post-exit `sacct`; the in-trap `slurm-final.txt` still shows
`RUNNING` because it predates the batch process exit.

The original benchmark code, SQL and inputs were preserved. The staged
SparkApplication manifests added only
`spark.kubernetes.jars.avoidDownloadSchemes=local` to avoid deleting the
image-local JAR during Spark's Kubernetes submission. Executor count and
image paths were environment overrides. The earlier pre-workload failure,
job 5357831, is excluded from these measurements.

Sources: [Skiff metrics and provenance](evidence/meluxina/spark-knot-skiff.json)
and [native Slurm metrics and provenance](evidence/meluxina/spark-knot-slurm.json).
Complete archives, logs, summaries, accounting and reports are retained locally
in the ignored directories
`analytics-spark/results/skiff/meluxina-5357975-collection/` and
`analytics-spark/results/slurm/meluxina-5358076-collection/`.
Both collection checksums and driver-log JSON records were checked. The Skiff
report was reproduced byte for byte; the native summaries passed the existing
native validator using local source-path mapping. Neither single run establishes
a speedup or platform overhead relative to the smaller Knot allocations.

## Dask matrix

Both Knot runs used Dask/Distributed 2024.1.0, NumPy 1.26.3, deterministic
8000 × 8000 matrices, 4000 × 4000 chunks, one Dask thread and two BLAS threads
per worker. Both executed 20 tasks and returned an 8000 × 8000 result of
488.28 MiB with mean **2000.165832**. Every worker participated.

| Metric | Skiff (3 nodes / 3 workers) | Native Slurm (2 nodes / 2 workers) |
| --- | ---: | ---: |
| Computation wall time | 36.137 s | 21.948 s |
| Aggregate task compute time | 33.018 s | 30.909 s |
| Aggregate task transfer time | 47.584 s | 18.211 s |
| Tasks by worker | 8 / 7 / 5 | 10 / 10 |
| Compute time by worker | 12.332 / 11.580 / 9.106 s | 15.392 / 15.518 s |
| Worker CPU samples | 216 | 90 |
| Maximum worker CPU | 279.7% | 244.2% |
| Mean worker CPU | 79.0% | 154.7% |

The Skiff client Job was `Complete`, exit `0`, with zero restarts or observed
OOMs; workers occupied all three nodes. Native Slurm job 635 used nodes 1 and
3, completed with exit `0:0` and launcher exit `0`, and took 33 s including
startup and cleanup. Its scheduler and client ran on node 1. An HTML
performance report was generated but is not versioned here.

Sources: [Skiff selected result](evidence/knot-platform/dask-matrix-skiff.json)
and [Slurm selected result](evidence/knot-platform/dask-matrix-slurm.json).

## BERT: interrupted after epoch 1

On 2026-10-15, the user requested cancellation after the first epoch and its
evaluation completed. This is a partial result, excluded from the successful
full-run tables above. Job `kubeflow/fine-tune-bert` used Slurm allocation 715,
with one CPU training process on each of `k8s-node1` and `k8s-node3`, four CPU
units and 16G memory per process. The workload was full fine-tuning of
`bert-base-cased`: 3600 training / 400 validation examples, sequence length
128, per-process batch 8, global batch 16, and three planned epochs / 675 steps.

| Observed first-epoch metric | Value |
| --- | ---: |
| Completed epoch | 1 of 3 |
| Steps at that epoch boundary | 225 of 675 |
| Validation loss | 1.1165157556533813 |
| Validation accuracy | 50.5% |
| Validation runtime | 32.3974 s |
| Time from training-start log to evaluation start | 3316.645 s (55m16.645s) |
| Time from training-start log through first evaluation metrics | 3349.043 s (55m49.043s) |

The epoch metrics were emitted at **10:58:24 UTC**. Training continued briefly
into the next epoch before cancellation; the exact final step is unknown.
The PyTorchJob was deleted and the Skiff allocation was signalled to stop at
11:00:49 UTC. Slurm ended the allocation at 11:01:21 UTC and released both
nodes. Slurm records `FAILED`, exit `143:0`, following the requested SIGTERM;
this was an intentional early stop. No checkpoint existed in `test_trainer`
at capture time, and final model export was not reached.

Source: [selected epoch metrics and provenance](evidence/knot-platform/bert-skiff-partial.json).
Logs, pre-cancellation job/pod state and the notebook were retained in remote
staging under `benchmarks/bert-notebook-knot/shutdown-20261015/`, with a local
ignored copy under `bert-distr-training/knot/artifacts/shutdown-20261015/`.

## Interpretation and retention

- Site, hardware, node and worker counts differ across these runs. They
  do not establish platform overhead or a speedup; a comparison requires
  matched topology, resources, storage, image/source identities and repeated
  independent runs.
- Spark task CPU utilization is aggregate task CPU time divided by aggregate
  task runtime, not whole-cluster utilization. Dask CPU percentages are
  sampled process metrics and can exceed 100%; they have a different definition.
- Aggregate task compute, transfer and wait times can overlap across workers
  and exceed wall time. Zero blocking shuffle fetch wait does not imply no
  network transfer. Spark workload wall time excludes session startup.
- Spark Knot is TPC-DS-derived, not an audited TPC result.
- Compact evidence contains selected original metrics, run identity and
  provenance. Raw logs, stage traces, HTML reports, pod dumps, credentials,
  model/data files and image archives are not part of the versioned results.
  Existing local and remote diagnostic artifacts are left in place.
- Older Vagrant Dask validation used different hardware and profiles; it is
  superseded here by the real Knot runs.
- BERT was intentionally stopped after epoch 1. It has no completed three-epoch
  result and is excluded from the successful tables. The current unified
  benchmark retains this measurement only as a sizing input; see its
  [README](bert-distr-training/README.md#one-hour-sizing-estimate).
