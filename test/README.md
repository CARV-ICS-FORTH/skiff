# Testing Skiff

The tests have three layers: Go regressions for the runtime and provider, shell regressions for supervision and test helpers, and a two-node end-to-end suite. The Vagrant environment supplies Slurm, Apptainer and the Linux networking tools needed by integration tests.

## Local Regressions

From the repository root:

```bash
make test
make test-race
make vet fmt-check check-shell
```

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

The qualification profile covers a dedicated two-node Slurm allocation with IPv4 TAP networking and kube-proxy Services. A required daemon failure stops the whole job; recovery uses a fresh job. Runtime qualification currently targets ARM64. Building for another architecture does not qualify its runtime behavior.

The profile requires these checks: cases 01–19, four individual daemon failure scenarios, TAP networking and admission checks, candidate identity, teardown with a second boot, and the named subcases listed in `test/e2e/lib/profile.sh`. They cover workload startup and completion, init containers, networking, readiness, environment and volumes, logs, exec and port-forward, restarts, graceful deletion, startup failures and cleanup, and the sprint 1 compatibility work:

| Case | Acceptance groups |
|---|---|
| 06 | T04 (kubelet routes and their failures) |
| 13 | T07, T08, T09 (startup, readiness and liveness probes, Service endpoints) |
| 14 | T14, T17 (exec, pseudo-terminals, cancellation) |
| 15 | T10, T11 (lifecycle hooks) |
| 16 | T12, T13 (logs and their attempts) |
| 17 | T18 (network settings that differ from the defaults, run as a fresh deployment) |
| 18 | T05, T06, T15, T16 (refused forms, updates, environment and files, continuity) |
| 19 | T20 (ten create/restart/delete cycles with probes, hooks, logs and exec, and what they leave behind) |

T01–T03, T19 and the unit and race parts of the other groups are unit and Linux tests (`make test` in `skifflet/` and `plaid/`; the Linux-only ones run in the builder container) and shell tests (`test/skiff-config-test.sh`, `test/skiff-supervision-test.sh`), run by `regression.sh` and the module test targets.

Supported probes are HTTP, TCP and exec (startup, readiness and liveness, with named ports); `grpc` probes, host namespaces, ephemeral containers, restartable init containers and the other forms in the support table of [skifflet/README.md](../skifflet/README.md) are refused at creation, and the refusal is tested with a healthy sentinel pod running beside them. Standalone Plaid checks NET-1 and NET-2 are outside this profile.

The default profile runs with the standard lab network. The alternative-network scenario (case 17, T18) is a separate fresh deployment, not a mode of the aggregate: source a file that exports the `SKIFF_*` network variables described in the top-level README (for example `SKIFF_POD_CIDR=10.200.0.0/16 SKIFF_SERVICE_CIDR=10.201.0.0/16 SKIFF_CLUSTER_DNS=10.201.0.10 SKIFF_CLUSTER_DOMAIN=lab.test SKIFF_POD_MTU=1400`), submit a new job, run the aggregate against it, and restore the standard configuration by submitting a new job without them.

T20 counts what disposable workloads can leave behind: application processes, the namespace's pod directories, IP leases, network namespaces, unpacked root filesystems, and the open descriptors of Skifflet and Plaidd (with an allowance of five). Volume data and image caches are retained on purpose and are not counted. Ten cycles is a regression run and not a measure of endurance.

Cases can also report named subcases with `e2e_subcase ID RESULT` (or run a function as one with `e2e_subcase_run ID cmd...`). The aggregate records each as a result of its own, named after the acceptance group and variant, for example `T07-startup-http`. Subcases the profile requires are listed in `test/e2e/lib/profile.sh`; a required subcase that is missing, skipped or failed fails the run, and a reported failure fails it even for a label not yet listed.

The fixture image also contains a controllable application (`fixture app serve|hook`, see `test/e2e/images/fixture/app.py`): readiness, startup and liveness switches that can be flipped over HTTP or through files for command probes, a start delay, per-path request counters, chosen exit codes, hook and SIGTERM event markers with timestamps and attempt numbers, and sequenced log records. Its behaviour is tested locally by `regression.sh`.

The aggregate saves `candidate.json`, `results.json` and logs in `~/skiff/test/e2e/results/<run-id>/` on the controller. Qualification requires a clean source commit, recorded image/fixture identities, matching runtime identity on both nodes, and every required check passing. A missing, failed or skipped required check fails the profile. Each aggregate uses a new result directory and fixed deployed artifacts throughout the run.
