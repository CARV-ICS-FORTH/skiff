# Comparing native Slurm and Skiff

Equal Spark core counts are task slots, not proof of equal effective resources.
Audit the executed processes and storage before interpreting a timing ratio.
The adapters share Python, SQL, generator image, metric aggregation, phase order
and query timers. They intentionally retain different cluster managers.

| Area | Potential mismatch | Comparison requirement |
| --- | --- | --- |
| CPU | Native bounded steps pin each worker, driver and MinIO to a small CPU set. Skiff uses requests for placement but does not enforce per-container CPU limits. | Use native `NATIVE_RESOURCE_MODE=whole-node` for the Skiff comparison; retain four Spark slots if that is the selected workload. Check inherited affinity and all visible cgroup ancestor quotas. |
| Memory | Native bounded steps have separate heap-plus-overhead limits; Skiff has no per-container memory enforcement. Equal Java heaps do not imply equal process memory envelopes. | Whole-node mode shares allocation memory with `--mem=0`; retain identical driver/executor heaps. Check memory cgroups and address-space limits. |
| JVM | Different CPU visibility can change JVM ergonomics even with identical task slots. | Record driver JVM available processors, effective CPU visibility and actual JVM command lines. Do not infer total CPU use from Spark task CPU counters. |
| Spark registration | Cluster managers have different default minimum registered resource ratios. This can move executor waiting across the session-start timing boundary. | Both adapters explicitly use `spark.scheduler.minRegisteredResourcesRatio=1.0`; the all-slot barrier still validates participation inside the workload timer. |
| Executed source | Skiff executes embedded Python/SQL; native binds host files. Equal SIF hashes alone do not establish equal executed source. | `source_identity.py` rejects differences in benchmark.py, runtime.py and all 103 SQL files before either backend runs. Record SIF and generator JAR hashes too. Both adapters set the executor PYTHONPATH to the embedded helper directory; verify imports with a real standalone worker, because local[2] shares the driver process layout. |
| Spark scratch | A default Skiff pod has 64 MiB `/dev/shm`, but Spark can overlay that path with a generated emptyDir. Native binds a host tmpfs. | Both adapters explicitly use `/spark-scratch`. Skiff uses an emptyDir; native uses `SPARK_SCRATCH_ROOT`. Verify the effective mount type, capacity and free space, rather than assuming that equal paths or emptyDir YAML imply equal storage. |
| MinIO backing | Native defaults to shared user storage. Skiff emptyDir inherits runtime scratch, potentially local tmpfs. | Select the same filesystem class and capacity on the same host; use fresh private directories and record actual mounts. |
| Executor working files/logs | Native standalone workers previously used shared result storage for executor working directories; Skiff uses local pod layers. | Native workers now use private node-local `/spark-work` and copy logs to durable results during cleanup, outside workload timing. Verify the underlying local filesystem. |
| Placement | Native driver, master and MinIO run on the first allocated node. Skiff scheduling otherwise chooses driver and MinIO hosts independently. | Set `CONTROL_NODE` to the first node's hostname label for the Skiff driver and MinIO. One executor per node is checked separately. |
| Images and environment | Java/Python/dependency versions, generator binaries, environment variables and home/tmp directories can differ despite similar launch commands. | Use the same SIF, capture versions and source identities; inspect effective Spark configuration. Keep transport, namespace and platform metadata differences explicit. |
| Timing | Both workload timers start after SparkSession creation, include the barrier/probes, workload validation and metrics collection, and stop before Spark shutdown. | Report generation, query and full allocation separately. Same-allocation sequential comparisons cannot provide independent allocation times for each backend. |
| Network | Native uses physical compute addresses. Skiff uses pod namespaces, Plaid overlay, service routing and DNS. | This remains an intended platform difference. Record transport, interface, MTU and service path; do not claim the execution environments are identical. |
| Platform activity | Skiff runs control plane, DNS, operator and networking services; native has standalone master/worker daemons. | Record versions and placement; include these in end-to-end platform timing. Do not disguise them by subtracting selected services. |
| Cache and topology | Different physical nodes, SMT/NUMA placement and execution order affect results. | Prefer both backends sequentially on the same exclusive nodes. Reverse order in an independent repetition when practical. No cache-dropping privilege or cold-cache claim is assumed. |
| Metrics | Skiff CPU/memory samples are process-tree accounting; native Slurm may report cgroup/step metrics. Spark task counters omit auxiliary activity. | Compare shared Spark metrics directly. Label whole-process, host and scheduler metrics by their source; do not equate task CPU utilization with whole-node utilization. |
| Image access | Skiff explicitly unpacks the SIF and adds a writable overlay in runtime scratch; direct native Apptainer chooses its default image mounting or unpacking path. | Record effective mountinfo and extraction locations. Matching SIF bytes does not establish equal image access or cache state; treat remaining container-runtime layout as a platform difference. |
| Security and filesystem semantics | Skiff accepts some securityContext fields without applying them. Container root/tmp layers and users differ from direct Apptainer execution. | Do not treat matching declarations as enforced isolation. Inspect actual identity, mount and limit evidence. This benchmark measures performance, not equivalent security isolation. |

## Native whole-node mode

```bash
NATIVE_RESOURCE_MODE=whole-node CPU_BIND=none \
    SPARK_INTERFACE=your-compute-interface SPARK_SIF=/shared/images/spark.sif \
    SPARK_SCRATCH_ROOT=/your/node-local/tmpfs \
    MINIO_DATA_ROOT=/your/node-local/tmpfs/private-minio ./slurm/submit.sh
```

This CPU-only mode requests the allocated CPU count for each overlapping role
step and shares allocation memory. It deliberately keeps Spark executor slots,
heap sizes and workload unchanged. It requires homogeneous allocated CPU counts.
The original `bounded` mode remains the default and is useful for deployments
that enforce per-role CPU/memory limits. These modes answer different questions.

For a matching Skiff deployment set `CONTROL_NODE`, choose runtime scratch on
the same backing filesystem, and inspect process snapshots in both phase JSON
summaries. The snapshots include affinity, cgroup membership and visible ancestor
limits, address-space limits, mount metadata and source hashes. A missing or
unresolvable limit record is unknown, not evidence of unlimited resources.

The admission and identity checks establish selected prerequisites. Successful
workload validation alone does not certify every row of this audit as matched.

References: [Skifflet support table](../../skifflet/README.md#kubernetes-support),
[Slurm step resource sharing](https://slurm.schedmd.com/srun.html), and
[Spark Kubernetes storage and resource settings](https://spark.apache.org/docs/4.0.4/running-on-kubernetes.html).
