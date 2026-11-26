# MeluXina benchmark results

Measurements from MeluXina. Results reflect the workload profile, hardware,
storage, and network used in each run. The ten-node Ray CPU comparison
summarizes five pairs. Other comparisons use one run per backend.

## Summary

| Benchmark | Nodes | Native workload time | Skiff workload time | Skiff/native workload | Native total job time | Skiff total job time | Skiff/native total |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Spark analytics, historical unequal CPU limits | 10 | 259.579 s | 191.683 s | 0.738× | 338 s | 465 s | 1.376× |
| Spark analytics, resource-parity rerun | 10 | 200.972 s | 191.585 s | 0.953× | — | — | — |
| Ray retail CPU, five-pair campaign (means) | 10 | 1251.90 s | 1214.05 s | 0.970× | 1276.60 s | 1322.40 s | 1.036× |
| Ray retail CPU, historical | 20 | 1521.836 s | 1483.617 s | 0.975× | 1549 s | 1644 s | 1.061× |
| BERT CPU, 3 epochs | 10 | 1808.898 s | 1846.017 s | 1.021× | 1829 s | 1996 s | 1.091× |
| BERT CPU, 12 epochs | 20 | — | 3882.588 s | — | — | 4162 s | — |
| Ray retail GPU | 5 | 2194.426 s | 2160.932 s | 0.985× | 2312 s (smoke + full) | 2297 s (smoke + full) | 0.994× |

The Spark resource-parity run used one shared **543 s** allocation. The
five-node Ray GPU allocations include an integrated smoke test. The twenty-node
BERT run has only a Skiff measurement.

## Timing definitions

- **Workload time** is measured by the benchmark and excludes platform startup
  and final cleanup: Spark is the sum of generation and query timers, Ray is
  the complete invocation timer (including application startup/shutdown), and
  BERT is the application timer (including imports and distributed setup).
  These scopes differ; compare within each benchmark. Ray phase sums are
  reported separately and exclude gaps outside the individual phase timers.
- **Non-workload time** is total job time minus workload time. It includes
  startup, validation, gaps outside the benchmark timer and cleanup.
- **Total job time** is the Slurm allocation from start to end. It excludes
  queue waiting. Slurm accounting end and observed allocation release are
  separate timestamps.

| Benchmark | Platform | Workload time | Non-workload time | Total job time |
| --- | --- | ---: | ---: | ---: |
| Spark analytics | Native Slurm | 259.579 s | 78.421 s | 338 s |
| Spark analytics | Skiff | 191.683 s | 273.317 s | 465 s |
| Ray retail CPU, 20 nodes | Native Slurm | 1521.836 s | 27.164 s | 1549 s |
| Ray retail CPU, 20 nodes | Skiff | 1483.617 s | 160.383 s | 1644 s |
| Ray retail CPU, 10 nodes (means) | Native Slurm | 1251.90 s | 24.70 s | 1276.60 s |
| Ray retail CPU, 10 nodes (means) | Skiff | 1214.05 s | 108.35 s | 1322.40 s |
| BERT CPU, 10 nodes | Native Slurm | 1808.898 s | 20.102 s | 1829 s |
| BERT CPU, 10 nodes | Skiff | 1846.017 s | 149.983 s | 1996 s |
| BERT CPU, 20 nodes | Skiff | 3882.588 s | 279.412 s | 4162 s |

## Spark analytics

### Resource-parity run

Both backends completed generation and queries on the
same ten exclusive CPU nodes. Native ran first, Skiff second, with fresh data
prefixes. The SF1 workload retained all 24 tables, 24 generation/shuffle
partitions, the same 16 queries × 3 repetitions, four task slots and 8 GiB heap
per executor, and four driver cores with 4 GiB heap. All ten executors
participated: 616 generation and 3195 query tasks per backend, with zero failed
Spark tasks. Query ordering and output row counts matched; complete query
result-value equality was not independently certified.

| Workload measurement | Native Slurm | Skiff | Skiff/native |
| --- | ---: | ---: | ---: |
| Generation | 93.991 s | 89.710 s | 0.954× |
| Queries | 106.981 s | 101.875 s | 0.952× |
| Combined workload | 200.972 s | 191.585 s | 0.953× |
| Generation task CPU utilization | 59.7% | 67.2% | — |
| Query task CPU utilization | 62.2% | 60.7% | — |
| Generation JVM GC | 11.337 s | 6.628 s | — |
| Query JVM GC | 7.611 s | 3.860 s | — |
| Query remote shuffle read | 136.75 MiB | 96.72 MiB | — |
| Query memory / disk spill | 0 B / 0 B | 0 B / 0 B | — |

The corrected post-run checker passed the retained evidence: driver and all
executor probes had CPU affinity **0–255**, driver JVM processor visibility
was **256**, NUMA memory nodes were **0–7**, address-space limits were unlimited,
and no finite CPU or memory limit appeared in the captured visible cgroup
ancestors. Source hashes, workload settings, topology and per-physical-node
scratch capacity matched. Four Spark task slots therefore did not impose a
four-CPU OS restriction on either backend. These observations cover visible
process state, rather than every possible site enforcement mechanism.

Native whole-node steps shared allocation memory and CPUs. Both used
`/spark-scratch` on node-local tmpfs; native executor working files/logs also
moved to local scratch. The driver and MinIO were co-located on `mel0050`;
MinIO was configured with local tmpfs backing on both sides. An independent
in-MinIO resource snapshot was not retained. All ten native preflight gates
verified that benchmark.py, runtime.py and all 103 SQL files matched the shared
application SIF. Both adapters explicitly matched the executor Python path
and resource-registration threshold.

The observed workload gap shrank from **26.2%** in the old pair to **4.7%**.
This is consistent with resource restrictions contributing to the old result,
but CPU affinity was not the only change: native MinIO moved from Lustre to
tmpfs, source/probe instrumentation changed, and scratch/log placement and
registration settings were matched. Network paths, platform services, image
unpacking and shuffle locality still differ. Native ran first and caches were
not controlled. One paired observation does not establish a causal CPU effect,
a general platform speedup or a statistical variation estimate.

| Controller timing boundary | Duration |
| --- | ---: |
| Native launcher through native cleanup | 239.424 s |
| Skiff cluster launch through supervisor cleanup | 296.521 s |
| Skiff workload wrapper only | 245.044 s |
| Shared Slurm allocation | 543 s |

The Skiff interval includes cluster startup, operator/MinIO installation,
preflight, both applications and cleanup. The workload wrapper is a nested
interval, not an additional cost. These controller intervals are separate from
the application timers and are not independent Slurm allocation times.

All four workload summaries passed. Slurm recorded exit **`1:0`** because the
post-run checker expected full pod names and Spark returned shortened
hostnames. A corrected offline checker validated placement, and the
allocation released after cleanup.

### Earlier comparison with unequal CPU limits

Both runs completed successfully. Each used ten CPU nodes, the same application image, benchmark
source, input and executor budget. Both phases passed validation, all 10
executors participated, and no Spark task failed. Spark 4.0.4 used TPC-DS scale
factor 1, all 24 tables, 24 generation/shuffle partitions and 16 ordered queries
repeated three times (48 executions). Each node hosted one four-core executor
with 8 GiB heap; the four-core driver had 4 GiB heap. That is 40 configured
executor cores within 2560 allocated logical CPUs. Both runs completed 616
generation and 3194 query tasks. This is a TPC-DS-derived workload, not an
audited TPC result.

| Measurement | Native Slurm | Skiff | Skiff/native |
| --- | ---: | ---: | ---: |
| Data generation | 141.639 s | 92.908 s | 0.656× |
| Queries | 117.940 s | 98.775 s | 0.837× |
| Combined workload | 259.579 s | 191.683 s | 0.738× |
| Non-workload time | 78.421 s | 273.317 s | 3.485× |
| Total job time | 338 s | 465 s | 1.376× |

Skiff used less time inside both workload phases. Its task CPU utilization was
also higher: 68.2% versus 52.0% during generation, and 62.5% versus 45.4%
during queries. These percentages describe time inside Spark tasks, not total
CPU use across the allocation.

The total job result went the other way. Skiff spent 194.896 s more outside the
workload timer, so its allocation lasted 127 s longer overall.

The configured Spark task slots match, but effective CPU restrictions do not.
The native launcher uses `srun --exact --cpu-bind=cores` and requests four CPUs
for each worker and driver. The retained native affinity records confirm masks
such as `0-1,128-129` for workers and `5-6,133-134` for the driver: four logical
CPU IDs per role. Native MinIO was similarly restricted. Workers launch their
executors as child processes within those steps.

Skiff declares four cores and a `4000m` container limit, but Skifflet does not
enforce per-container CPU limits or affinity; its bubble step requests all node
CPUs. Four Spark
task slots therefore do not establish the same OS CPU budget on both backends.
The retained Skiff evidence does not include executor/driver affinity and quota
measurements, so actual CPU availability and extra CPU consumption cannot be
quantified retrospectively. The binding mismatch is a plausible contributor,
not a measured explanation of the timing gap.

Native Spark also used the physical InfiniBand interface and shared Lustre
storage for the data service; Skiff used virtual networking and node-local
tmpfs. These CPU, storage and network differences prevent interpreting the
ratios as platform overhead or a controlled speedup. A matched rerun should
keep Spark task slots unchanged and verify actual CPU masks and cgroup quotas
inside both backends, including the driver and MinIO. Setting `CPU_BIND=none`
alone is not evidence of equal CPU availability.

The top-level Slurm jobs and benchmark validators succeeded. Native service
steps and Skiff cluster steps were stopped during planned teardown. The saved
Skiff application records marked executors failed despite completed
applications; driver exit was zero and no Spark task failed, but clean
executor shutdown was not independently confirmed.

[Run the Spark analytics example](analytics-spark/README.md).

## Ray retail CPU

### Ten-node, five-pair campaign

The campaign ran five native Slurm and five official Skiff 0.1.0
measurements in sequential allocations, each on ten exclusive CPU
nodes. The workload and resource budgets were frozen across backends. Pilots
were excluded. All ten applications completed. Nine measurements passed normal
validation. Pair 1 Skiff was accepted with an explicitly authorized cleanup-only
exception for one node’s bubble cleanup verification.

| Metric | Native mean ± sample SD | Skiff mean ± sample SD |
| --- | ---: | ---: |
| Application time | 1251.90 ± 13.53 s | 1214.05 ± 14.97 s |
| Slurm allocation time | 1276.60 ± 14.15 s | 1322.40 ± 25.17 s |
| Allocation start to application start | 21.35 ± 0.84 s | 68.49 ± 1.70 s |
| Application end to backend stopped | 3.81 ± 0.18 s | 40.56 ± 11.86 s |
| Whole-node physical CPU busy during application | 31.03 ± 0.30% | 31.55 ± 0.28% |

The mean of the five paired application-time ratios was **0.9699**, or
**3.01% less application time** on Skiff. The mean paired difference (Skiff minus native) was **−37.85 s**, with a two-sided
Student-t 95% confidence interval of **−67.84 to −7.86 s**. Skiff's mean
allocation was **45.80 s longer (3.59%)**. Excluding pair 1 as a sensitivity check
leaves four normally validated pairs with a mean paired ratio of **0.9737**
and difference of **−33.17 s**.

Application time spans the adapter's application start and finish, including
setup and finalization. Native used Gloo/TCP over IPoIB, while Skiff used Plaid's
RDMA overlay. The comparison covers both platforms and their network paths.
Cleanup statistics include the accepted exception.

| Pair | Native application (s) | Skiff application (s) | Skiff − native (s) | Native allocation (s) | Skiff allocation (s) |
|---|---:|---:|---:|---:|---:|
| 1* | 1252.45 | 1195.89 | -56.56 | 1278 | 1283 |
| 2 | 1271.40 | 1214.09 | -57.31 | 1297 | 1325 |
| 3 | 1246.40 | 1231.23 | -15.17 | 1271 | 1344 |
| 4 | 1254.99 | 1202.87 | -52.12 | 1279 | 1316 |
| 5 | 1234.24 | 1226.16 | -8.08 | 1258 | 1344 |

*Pair 1 includes the accepted Skiff cleanup exception.

Mean phase times and sample standard deviations across the five pairs are below.

| Phase | Native mean ± SD (s) | Skiff mean ± SD (s) |
|---|---:|---:|
| data | 575.15 ± 5.57 | 565.96 ± 5.50 |
| tune | 38.65 ± 2.14 | 36.11 ± 4.51 |
| train | 191.20 ± 3.49 | 183.06 ± 4.40 |
| batch | 48.42 ± 0.53 | 42.52 ± 1.15 |
| policy | 179.83 ± 2.74 | 175.50 ± 2.65 |
| evaluation | 12.86 ± 0.21 | 12.46 ± 0.15 |
| serve | 193.74 ± 8.20 | 188.17 ± 5.61 |
| recovery | 3.00 ± 1.16 | 3.20 ± 0.04 |

### Historical twenty-node runs

Both runs completed successfully on twenty CPU nodes each. Source,
configuration, package versions,
application image, Ray resource limits and generated input checksums matched.
All recorded phase artifacts passed checksum validation.

| Phase | Native Slurm | Skiff | Skiff/native |
| --- | ---: | ---: | ---: |
| Data generation | 233.762 s | 218.154 s | 0.933× |
| Parameter tuning | 132.210 s | 128.430 s | 0.971× |
| Distributed training | 176.403 s | 204.090 s | 1.157× |
| Batch inference | 13.186 s | 12.850 s | 0.975× |
| Policy training | 401.905 s | 389.195 s | 0.968× |
| Policy evaluation | 7.647 s | 7.308 s | 0.956× |
| Online serving | 551.230 s | 518.634 s | 0.941× |
| Recovery test | 3.396 s | 3.143 s | 0.926× |
| Complete workload | 1521.836 s | 1483.617 s | 0.975× |
| Non-workload time | 27.164 s | 160.383 s | 5.904× |
| Total job time | 1549 s | 1644 s | 1.061× |

Skiff was faster in every measured phase except distributed training. Across
the complete workload it was 2.5% faster, but time outside the application timer
made the total allocation 6.1% longer.

The serving results show the same pattern:

| Serving metric | Native Slurm | Skiff |
| --- | ---: | ---: |
| Throughput | 111.830 requests/s | 118.738 requests/s |
| Median latency | 395.793 ms | 379.557 ms |
| p95 latency | 954.462 ms | 889.185 ms |
| p99 latency | 1067.283 ms | 965.198 ms |
| Requests / client errors | 60,000 / 0 | 60,000 / 0 |

Forecast test MAE was 4.9544 native / 4.9626 Skiff, against the same 9.6965
seasonal baseline; WAPE was 0.13161 / 0.13183. Each run processed 6.72 million
training rows, six tuning trials, 16 training epochs over 20 ranks, and 120 PPO
iterations / 1,966,080 sampled steps. Ray exposed 32 CPUs per node (640 total)
within 5120 allocated logical CPUs, with a 16 GiB object store and 60 GiB heap
budget per node. Twenty forecast and twenty policy replicas served requests.
The summed phase times were 1519.738 s native / 1481.804 s Skiff, distinct from
the complete invocation timers above.

Sampled resource use was similar:

| Resource metric | Native Slurm | Skiff |
| --- | ---: | ---: |
| Mean host CPU | 1.666% | 1.809% |
| Peak synchronized mean host CPU | 12.860% | 12.975% |
| Peak visible Ray-process RSS / PSS | 180.53 / 117.88 GiB | 178.53 / 115.92 GiB |
| Peak host memory used | 207.68 GiB | 216.04 GiB |

Both runs completed every phase and validated their results. The Skiff logs
contained seven internal serving-proxy errors, but requests around them still
returned successfully and the benchmark recorded zero client errors. The
worker termination in the logs belongs to the planned recovery test.

The comparison tool did not certify this pair as fully like-for-like. Both runs
limited Ray's object store to 16 GiB per node, but the underlying temporary
filesystems exposed different total capacities: about 427.70 GiB per native
node and 251.59 GiB per Skiff node. The phase ratios therefore remain useful
observations, not a controlled speedup claim.

## Ray retail GPU

Both backends completed all eight phases with exit **`0:0`**. Each
allocation ran an all-node smoke, validated its artifacts, released the
smoke's Ray GPU reservations, and then ran the full workload.

| Time boundary | Native Slurm | Skiff |
| --- | ---: | ---: |
| Smoke application | 79.891 s | 62.644 s |
| Full application | 2194.426 s | 2160.932 s |
| Sum of full-workload phases | 2188.215 s | 2155.164 s |
| Slurm allocation, including both stages | 2312 s | 2297 s |

Each exclusive node reserved four NVIDIA A100-SXM4-40GB GPUs and 128 logical
CPUs. Ray exposed **one GPU and 32 CPUs per node**, for five usable Ray GPUs
and 160 Ray CPUs within each 20-GPU and 640-CPU allocation. PPO used one GPU
learner. Each Ray node used a 16 GiB tmpfs object store and a 60 GiB heap
budget, with shared Lustre scratch for results and data. Native communication
used `ib0`.

The resolved five-node GPU profile used 10,000 series, 1000 days and 400 raw
blocks: **6.72 million training, 1.5 million validation and 1.5 million test
rows**. Six Tune trials each completed six epochs. Final training used five
CUDA/NCCL ranks for all 16 epochs, each reporting **21,495,808 samples** and
1312 steps per epoch. PPO completed 120 iterations / 1,966,080 sampled steps
with verified checkpoint restoration, followed by 1200 evaluation episodes
per policy. HTTP serving used five forecast and five policy replicas, and
all ten replicas handled requests.

Forecast test MAE was **4.9868**, versus **9.6965** for the seasonal baseline;
WAPE was **0.13248**. Serving completed **60,000 requests with zero errors**,
verified against direct inference, in **477.316 s** of load generation:
**125.703 requests/s**, with p50/p95/p99 latency **344.959 / 831.707 / 893.388 ms**.
The serving phase includes deployment and teardown. Recovery intentionally
terminated one actor and verified restored and duplicate totals of 18.

The image used Ray 2.49.2, PyTorch 2.8.0+cu128 and CUDA 12.8. CPU data
preparation accounts for about half the full phase time. Ray Serve reported
that its default per-replica `max_ongoing_requests=5` constrained the configured
batch size of 32. The integrated smoke warms libraries, GPUs and caches;
cold-start behavior was not isolated.

The two GPU runs matched the profile, source, dependencies, CUDA SIF, Ray
budgets, checksums for all 400 raw blocks, and CPU affinity **0–15,64–79**.
The Skiff run passed 435 smoke and 1395 full-workload artifact checks.
Placement was reconstructed from Ray addresses and node PodCIDRs. Application
logs were retained. A cleanup path error prevented final pod-log and event
collection, and the cluster shut down and released its allocation.

| Full workload phase | Native seconds | Skiff seconds |
| --- | ---: | ---: |
| Data preparation | 1095.123 | 1081.305 |
| Tune | 100.529 | 99.051 |
| Five-rank NCCL training | 85.972 | 81.007 |
| GPU batch inference | 7.789 | 7.395 |
| PPO training and restore | 391.740 | 384.978 |
| Policy evaluation | 16.388 | 16.179 |
| HTTP serving | 488.938 | 483.888 |
| Actor recovery | 1.735 | 1.363 |

Skiff completed 60,000 HTTP requests with zero errors at **126.513 requests/s**.
Its full application time was **33.494 s (1.53%) shorter** in this single pair.
The built-in comparison **refused certification** because host tmpfs capacities
vary slightly between the physical nodes, despite matching tmpfs backing type,
16 GiB object-store budgets and image identities. These ratios are observational,
not a harness-certified comparison or evidence of a general platform speedup.
The GPU and twenty-node CPU profiles use different hardware, rank counts
and software, so these timings cannot quantify GPU acceleration.

[Run the Ray retail example](ray-retail/README.md).

## BERT CPU training

### Matched ten-node runs

Both runs completed with allocation and benchmark exits zero. Each used ten exclusive CPU nodes,
one rank per node, 32 PyTorch threads per rank and verified affinity to all
256 logical CPUs per rank. Source hashes, CPU image, dependencies and assets
matched: `bert-base-cased`, Yelp Review Full, 28,800 training / 3200 validation
examples, sequence length 128, three epochs, global batch 80 and 1080 steps.

| Measurement | Native Slurm | Skiff | Skiff/native |
| --- | ---: | ---: | ---: |
| Training (including epoch evaluation) | 1792.691 s | 1833.706 s | 1.023× |
| Complete application | 1808.898 s | 1846.017 s | 1.021× |
| Outside application timer | 20.102 s | 149.983 s | 7.461× |
| Total job time | 1829 s | 1996 s | 1.091× |
| Training throughput | 48.214 samples/s | 47.133 samples/s | 0.978× |
| Final validation accuracy | 58.84375% | 58.84375% | — |
| Final validation loss | 1.056114 | 1.056114 | — |

Mean training loss (0.817420) and all three epoch losses/accuracies matched.
Skiff's ten pods completed with zero restarts. Native training and batch steps
completed with exit `0:0`; model export was retained but not independently
re-evaluated. The comparison uses the native run with whole-node affinity.

The 41.015 s training difference is 2.29% of native time. Physical nodes and
start times differed; caches, NUMA policy and transport were not controlled.
The application difference is an observation from one run per platform.

### Twenty-node run and sizing comparison

The Skiff run completed successfully on twenty CPU nodes.
All 20 training pods completed on different nodes with zero restarts and zero
container errors. The result validator passed.

| Time boundary | Duration |
| --- | ---: |
| Cluster ready after allocation start | 117 s |
| Full startup to measured application | approximately 144–145 s |
| PyTorchJob lifetime | 3892 s (1h04m52s) |
| Measured application | 3882.588 s (1h04m42.588s) |
| Training | 3869.677 s (1h04m29.677s) |
| Post-application validation and cleanup | approximately 134–135 s |
| Total Slurm job time | 4162 s (1h09m22s) |

The 279.412 s outside the application timer was 6.71% of the allocation. At
the recorded wrapper boundaries it splits into 134 s before benchmark launch,
21.412 s inside the launcher but outside the application timer, and 124 s of
cluster teardown.

| Result | Value |
| --- | ---: |
| Training throughput | 89.326 samples/s |
| Final training loss | 0.343162 |
| Final validation loss | 2.219731 |
| Final validation accuracy | 57.6875% |
| Best validation accuracy | 59.0000% at epoch 4 |
| Lowest validation loss | 0.951355 at epoch 2 |

An earlier 10-node MeluXina Skiff run took 1833.706 s for three epochs and
processed 47.133 samples/s. The 20-node run processed four times as many
examples across epochs on twice as many nodes. It took 2.110 times as long and
delivered 1.895 times the throughput. This is useful for sizing, but it is not
a repeated scaling study. The runs also used different global batch sizes, so
their validation results do not isolate the effect of the longer schedule.

This run used the same 28,800 training / 3200 validation examples and
sequence length 128, with 12 epochs, one rank and 32 CPU threads per node,
64 GiB requested memory per pod, global batch 160 and 2160 optimizer steps.
There is no matching twenty-node native result; the ten-node native run above
uses a different epoch count and batch size.

[Run the BERT fine-tuning example](bert-distr-training/README.md).
