# Testing Skiff

The tests have three layers: Go regressions for the runtime and provider, shell regressions for supervision and test helpers, and a two-node end-to-end suite. The Vagrant environment supplies Slurm, Apptainer and the Linux networking tools needed by integration tests.

## Local Regressions

From the repository root:

```bash
make test
make test-race
make vet fmt-check check-shell
```

`make check-shell` runs shellcheck (the builder image's, so one version everywhere; a missing shellcheck is a failure, not a skip) on every script, warnings included except `SC2034`. `test/regression.sh` also runs `test/pipefail-guard.sh`, which fails on a pipeline of a `pipefail` script that ends in a reader that stops early (`grep -q`, `head`, `awk ... exit`): the writer dies of SIGPIPE and the pipeline fails although the answer was there, which once made the supervisor end a job whose bubble was alive. A line that is safe for a reason carries `pipefail-ok` in a comment.

`make test-race` runs both modules' tests with the race detector inside the builder container (`images/skiff-builder`: Ubuntu 24.04 with an exact, checksummed Go release and a C compiler), as an unprivileged user and on Linux whatever the host is, so the Linux-only tests (terminals, namespaces, process trees, directory modes) run too and are not skipped. The builder's image id and Go version are recorded in the candidate manifest and checked by the qualification. Docker is the only host prerequisite.

Some shell regressions require Bash 4 or later. On systems with an older Bash, run the Linux regressions in the controller VM as described below.

## VM Tests

Start the Vagrant environment following the top-level README, then build and deploy both the bubble and test fixture images:

```bash
make develop e2e-images
make -C test/vagrant test
make -C test/vagrant e2e
```

The test Makefile accesses the controller over SSH. Its `test` target runs the shell regressions; `e2e` starts a development cluster using the locally built image, runs the aggregate suite, and cleans up. To synchronize edited scripts and tests without rebuilding images, use `make -C test/vagrant sync` between runs.

### Running Manually in the Controller VM

Connect with `ssh vagrant@controller.local`, or use `vagrant ssh controller` from `test/vagrant/`. After deploying the images:

```bash
cd ~/skiff
bash test/regression.sh
SKIFF_DEV=1 bash test/e2e/run-all.sh
```

The aggregate starts its own Slurm job. For a focused test against an already running development cluster, pass its job ID:

```bash
export KUBECONFIG=${HOME}/.skiff/kubeconfig
JOB_ID=123  # Replace with your cluster's Slurm job ID.
SKIFF_DEV=1 bash test/e2e/run-all.sh --job-id "$JOB_ID" --case 07 --no-teardown
```

Focused runs help with development; they do not qualify the full profile. Case 09 injects a daemon failure and stops its cluster job. The individual TAP networking checks can also be run against a running cluster:

```bash
bash test/e2e/network/test-tap-acceptance.sh
bash test/e2e/network/test-tap-webhook.sh
```

## Qualification Profile and Results

The qualification profile covers a dedicated two-node Slurm allocation with IPv4 TAP networking with Plaid translating Services. A required daemon failure stops the whole job; recovery uses a fresh job. Runtime qualification currently targets ARM64. Building for another architecture does not qualify its runtime behavior.

The profile requires these checks: cases 01–30, five individual daemon failure scenarios (the controller's K3s too; each also checks that the data of a claim survives the failed job and that the first failure is recorded with the process and how it ended, T36), TAP networking and admission checks, candidate identity, teardown with a second boot, and the named subcases listed in `test/e2e/lib/profile.sh`. They cover workload startup and completion, init containers, networking, readiness, environment and volumes, logs, exec and port-forward, restarts, graceful deletion, startup failures and cleanup, and the sprint 1 compatibility work:

| Case | Acceptance groups |
|---|---|
| 05 | T27 (a pod waits for a claim that is not bound for longer than the old fixed wait, with the reason and one event, and starts when the volume appears; a waiting pod holds nothing up and is deleted at once) |
| 06 | T04 (kubelet routes and their failures) |
| 13 | T07, T08, T09 (startup, readiness and liveness probes, Service endpoints) |
| 14 | T14, T17 (exec, pseudo-terminals, cancellation) |
| 15 | T10, T11, T26 (lifecycle hooks; unrelated pods are created while several pods are inside a long PreStop hook) |
| 16 | T12, T13 (logs and their attempts) |
| 17 | T18, T32 (network and scratch-directory settings that differ from the defaults, run as a fresh deployment; where Apptainer's scratch really is during an image pull, and where a volume may be) |
| 18 | T05, T06, T15, T16 (refused forms, updates, environment and files, continuity) |
| 27 | T34 (the writable layer of a container: 300 MiB written to the root filesystem and read back, in the image's own directories and in new ones, the same in every exec, found on the node's disk below the pod's directory and gone with the pod; private to a container, an init container and a later pod; empty at every restart, with only the running attempt's layer on the node) |
| 28 | T37 (usage: the kubelet's resource metrics and summary carry the node and the containers, a burning container's CPU counter grows by about a core per second and a container that holds 150 MiB shows it; metrics-server is available without anyone installing it, `kubectl top node` and `kubectl top pod` show both nodes and the containers, and a deleted pod disappears from them) |
| 29 | T38 (a container that runs as another user than root is in the pod's network, not the bubble's, and exec runs as that user; the securityContext fields Skifflet does not apply are accepted and named in a Warning event; a volume is writable by a user that is not root; runAsNonRoot alone runs as nobody) |
| 30 | T40 (what a node offers and a pod asks for: a node advertises the CPUs and the memory it has, pods are placed by their requests, one that fits no node stays Pending with the scheduler's reason, and a limit is accepted and not enforced) |
| 25 | T27, T28, T29 (the default StorageClass: a claim without a class is bound, its directory is below the job's own on the shared home and what a pod writes is there; explicit and empty classes; no quota; a provisioner that is away for longer than the old wait; a refused creation and its recovery; replacement pods on the other node and a ReadWriteMany claim; a local volume keeps its pod on its node; Delete removes the directory, a claim in use is protected, a volume patched to Retain keeps it, a failed deletion is retried, a user's own paths are never touched) |
| 26 | T30, T39 (T39: the job is told to end with `scancel --batch --signal=TERM` while a pod on each node has a preStop hook and a SIGTERM handler that takes a few seconds: both ran and finished before the job ended, within the time Slurm leaves, with the exit status of a stop and the cause recorded) and T30 (a new job, with jobs of its own: the old data is still there below a directory named after the old job, the new cluster has none of the old objects, and a claim of the same namespace and name gets an empty directory of its own) |
| 24 | T31 (the temporary files of a container: its own `/tmp` and `/var/tmp` on the node's disk, empty at every attempt, not shared with another container, not the node's, gone with the pod; emptyDir volumes share files, also across a restart) |
| 19 | T20 (ten create/restart/delete cycles with probes, hooks, logs and exec, and what they leave behind) |
| 20 | T21 (the overlay socket: held by plaidd in the host's network namespace, no slirp forward for it, no helper or handoff socket left, cross-node traffic moves the overlay counters) |
| 21 | T22, T32 (overlay socket startup failures, with jobs of its own: cancel while the helper holds the port, helper killed, port taken, a scratch directory that cannot be used, and a fresh start afterwards) |
| 22 | T23, T24 (Service translation by Plaid with no kube-proxy: TCP and UDP, named ports, local, remote, hairpin and non-pod backends, DNS over UDP and TCP and a moved CoreDNS, the bubbles as clients, large responses, headless and ExternalName left alone, flows pinned through endpoint changes, refusals, unsupported settings, nothing dropped as unsupported) |
| 23 | T25 (NodePort Services published through slirp4netns on every node, from the other node's host: TCP, UDP, a node without a local backend, the forwards and the fixed ones, port changes and recreated Services, no backends, a connection that outlives its withdrawn node port, a node port another process holds and its recovery, the Local policy and LoadBalancer Services refused with events, drift repaired, nothing left behind) |

T01–T03, T19 and the unit and race parts of the other groups are unit and Linux tests (`make test` in `skifflet/` and `plaid/`; the Linux-only ones run in the builder container) and shell tests (`test/skiff-config-test.sh`, `test/skiff-supervision-test.sh`, `test/skiff-bubble-test.sh`, which runs the bubble script against a fake slirp4netns API and a fake Apptainer), run by `regression.sh` and the module test targets.

Supported probes are HTTP, TCP and exec (startup, readiness and liveness, with named ports); `grpc` probes, host namespaces, ephemeral containers, restartable init containers and the other forms in the support table of [skifflet/README.md](../skifflet/README.md) are refused at creation, and the refusal is tested with a healthy sentinel pod running beside them. Standalone Plaid, NET-1 (the CNI plugin against a standalone `plaidd`) and NET-2 (`plaidd` and `plaidtainer` on their own, as an ordinary user, on both nodes), is part of the profile: the binaries are taken from the uploaded image for the duration of the two suites and removed again (`e2e/lib/standalone.sh`), so the candidate under test is what runs. See [plaid/README.md](../plaid/README.md).

The default profile runs with the standard lab network. The alternative-network scenario (case 17, T18) is a separate fresh deployment, not a mode of the aggregate: source a file that exports the `SKIFF_*` network variables described in the top-level README (for example `SKIFF_POD_CIDR=10.200.0.0/16 SKIFF_SERVICE_CIDR=10.201.0.0/16 SKIFF_CLUSTER_DNS=10.201.0.10 SKIFF_CLUSTER_DOMAIN=lab.test SKIFF_POD_MTU=1400 SKIFF_TMP_DIR=/var/tmp/skiff-alt`; a scratch directory outside `/tmp` also checks that the bubbles are given it), submit a new job, run the aggregate against it, and restore the standard configuration by submitting a new job without them.

T20 counts what disposable workloads can leave behind: application processes, the namespace's pod directories, IP leases, network namespaces, unpacked root filesystems, and the open descriptors of Skifflet and Plaidd (with an allowance of five). Volume data and image caches are retained on purpose and are not counted. Ten cycles is a regression run and not a measure of endurance.

Cases can also report named subcases with `e2e_subcase ID RESULT` (or run a function as one with `e2e_subcase_run ID cmd...`). The aggregate records each as a result of its own, named after the acceptance group and variant, for example `T07-startup-http`. Subcases the profile requires are listed in `test/e2e/lib/profile.sh`; a required subcase that is missing, skipped or failed fails the run, and a reported failure fails it even for a label not yet listed.

The fixture image also contains a controllable application (`fixture app serve|hook`, see `test/e2e/images/fixture/app.py`): readiness, startup and liveness switches that can be flipped over HTTP or through files for command probes, a start delay, per-path request counters, chosen exit codes, hook and SIGTERM event markers with timestamps and attempt numbers, and sequenced log records. Its behaviour is tested locally by `regression.sh`.

The aggregate saves `candidate.json`, `results.json` and logs in `~/skiff/test/e2e/results/<run-id>/` on the controller. Qualification requires a clean source commit, recorded image/fixture identities, matching runtime identity on both nodes, and every required check passing. A missing, failed or skipped required check fails the profile, and so does a failed result that the profile does not require. The verdict line says what was covered: a focused run (`--case`) reports that it did not check the profile. Each aggregate uses a new result directory and fixed deployed artifacts throughout the run.
