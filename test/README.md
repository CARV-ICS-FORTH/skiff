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

## Alpha Profile and Results

The `skiff-tap-alpha` profile covers a dedicated two-node Slurm allocation with IPv4 TAP networking and kube-proxy Services. A required daemon failure stops the whole job; recovery uses a fresh job. Runtime qualification currently targets ARM64. Building for another architecture does not qualify its runtime behavior.

The profile requires 20 checks: cases 01–12, four individual daemon failure scenarios, TAP networking and admission checks, candidate identity, and teardown with a second boot. They cover workload startup and completion, init containers, networking, readiness, environment/volumes, logs/exec/port-forward, restarts, graceful deletion, startup failures and cleanup. Standalone Plaid checks NET-1 and NET-2 are outside this profile.

Supported probes are numeric-port HTTP/TCP readiness and liveness, plus command readiness. Startup probes, command liveness, gRPC/named-port probes and lifecycle hooks are rejected. Host namespaces and ephemeral containers are also rejected before local allocation.

The aggregate saves `candidate.json`, `results.json` and logs in `~/skiff/test/e2e/results/<run-id>/` on the controller. Qualification requires a clean source commit, recorded image/fixture identities, matching runtime identity on both nodes, and every required check passing. A missing, failed or skipped required check fails the profile. Each aggregate uses a new result directory and fixed deployed artifacts throughout the run.
