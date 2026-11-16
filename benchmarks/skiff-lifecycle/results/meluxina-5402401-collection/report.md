# Skiff lifecycle benchmark report

- Slurm job: `5402401`
- Requested nodes: 20
- Timestamp pods observed: 20
- Skiff version: `0.1.0-a9da5aa-lifecycle`
- Source commit: `a9da5aa1c7ba2c1c460320a9ec7002bea577ed57+command-timing`
- Skiff image SHA-256: `413a80d7c4796f4285d956a76d7f0e78896528a2da2b8fad8ce5ff3f80085ad6`
- Lifecycle validation: **FAIL** — 0/20 entrypoints completed cleanup
- Workload: one long-lived BusyBox timestamp pod per Kubernetes node

## Main timings

| Boundary | Duration |
| --- | ---: |
| Slurm queue (submit to allocation) | 1.000 s |
| Allocation to Skiff supervisor | 4.846 s |
| Network discovery | 8.381 s |
| Node preflight | 46.565 s |
| Skiff image preparation | 0.008 s |
| Controller initialization gate | 11.028 s |
| Worker launch calls | 0.280 s |
| Total allocated lifetime | 183.000 s |
| Allocation to all Kubernetes Nodes Ready | 125.693 s |
| Allocation to all Skiff entrypoints Ready | 125.745 s |
| Manifest apply | 0.289 s |
| Skiff entrypoints Ready to timestamp Pods Ready | 3.627 s |
| Manifest apply to timestamp Pods Ready | 3.615 s |
| Shutdown request delivery | 0.057 s |
| Skiff supervisor cleanup | 48.875 s |
| Requested shutdown to allocation end | 48.717 s |

## Instrumented commands and waits

These are the command or polling spans that have matching start/end markers,
sorted by duration. Aggregate rows contain multiple commands or a launch plus
a readiness wait; they must not be attributed to one executable.

| Phase | Target | Command or wait | Measurement | Duration |
| --- | --- | --- | --- | ---: |
| startup | `mel0047` | `launch controller bubble; poll for kubeconfig and server CA` | aggregate gate | 11.028 s |
| startup | `mel0047` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 6.388 s |
| shutdown | `mel0417` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 5.836 s |
| startup | `mel0047` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 5.760 s |
| shutdown | `mel0533` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 5.495 s |
| startup | `mel0047` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 5.145 s |
| startup | `mel0047` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 5.126 s |
| startup | `mel0057` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.115 s |
| startup | `mel0132` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.115 s |
| startup | `mel0099` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.110 s |
| startup | `mel0134` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.080 s |
| startup | `mel0159` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.071 s |
| startup | `mel0144` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.054 s |
| startup | `mel0230` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.051 s |
| startup | `mel0220` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.032 s |
| startup | `mel0336` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 5.018 s |
| startup | `mel0244` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.989 s |
| startup | `mel0386` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.973 s |
| startup | `mel0348` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.949 s |
| startup | `mel0404` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.935 s |
| startup | `mel0506` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.916 s |
| startup | `mel0429` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.915 s |
| startup | `mel0518` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.910 s |
| startup | `mel0422` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.900 s |
| startup | `mel0417` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.884 s |
| startup | `mel0533` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 4.873 s |
| startup | `mel0047` | `apptainer instance run ...` | command | 4.443 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get pods; kubectl logs for every timestamp pod` | aggregate commands | 4.411 s |
| startup | `mel0047` | `start k3s server; poll for /etc/rancher/k3s/k3s.yaml` | command + wait | 4.224 s |
| shutdown | `mel0159` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 3.880 s |
| pod setup | `cluster` | `poll DaemonSet status until one timestamp pod per node is Ready` | polling wait | 3.327 s |
| shutdown | `mel0404` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 3.110 s |
| startup | `mel0386` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 3.080 s |
| startup | `mel0159` | `poll for the controller-signed kubelet certificate` | polling wait | 3.026 s |
| startup | `mel0230` | `poll for the controller-signed kubelet certificate` | polling wait | 3.025 s |
| startup | `mel0518` | `poll for the controller-signed kubelet certificate` | polling wait | 3.025 s |
| startup | `mel0417` | `poll for the controller-signed kubelet certificate` | polling wait | 3.024 s |
| startup | `mel0422` | `poll for the controller-signed kubelet certificate` | polling wait | 3.024 s |
| startup | `mel0429` | `poll for the controller-signed kubelet certificate` | polling wait | 3.024 s |
| startup | `mel0533` | `poll for the controller-signed kubelet certificate` | polling wait | 3.023 s |
| startup | `mel0386` | `poll for the controller-signed kubelet certificate` | polling wait | 3.023 s |
| startup | `mel0336` | `poll for the controller-signed kubelet certificate` | polling wait | 3.022 s |
| startup | `mel0506` | `poll for the controller-signed kubelet certificate` | polling wait | 3.022 s |
| startup | `mel0348` | `poll for the controller-signed kubelet certificate` | polling wait | 3.022 s |
| shutdown | `mel0220` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 3.009 s |
| shutdown | `mel0047` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.787 s |
| startup | `mel0144` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.753 s |
| startup | `mel0230` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.747 s |
| startup | `mel0057` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.737 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.734 s |
| startup | `mel0336` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.716 s |
| startup | `mel0099` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.715 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.708 s |
| startup | `mel0134` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.689 s |
| startup | `mel0386` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.683 s |
| startup | `mel0159` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.679 s |
| startup | `mel0348` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.674 s |
| startup | `mel0244` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.664 s |
| startup | `mel0404` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.653 s |
| startup | `mel0422` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.632 s |
| startup | `mel0429` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.628 s |
| startup | `mel0506` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.621 s |
| startup | `mel0417` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.610 s |
| startup | `mel0518` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.598 s |
| startup | `mel0533` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 2.563 s |
| startup | `mel0057` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.470 s |
| shutdown | `mel0429` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.427 s |
| startup | `mel0422` | `poll plaidctl status` | polling wait | 2.370 s |
| startup | `mel0506` | `poll plaidctl status` | polling wait | 2.284 s |
| startup | `mel0144` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.263 s |
| startup | `mel0132` | `poll plaidctl status` | polling wait | 2.259 s |
| startup | `mel0220` | `poll plaidctl status` | polling wait | 2.257 s |
| startup | `mel0047` | `poll plaidctl status` | polling wait | 2.250 s |
| startup | `mel0057` | `poll plaidctl status` | polling wait | 2.247 s |
| startup | `mel0533` | `poll plaidctl status` | polling wait | 2.245 s |
| startup | `mel0230` | `poll plaidctl status` | polling wait | 2.245 s |
| startup | `mel0244` | `poll plaidctl status` | polling wait | 2.245 s |
| startup | `mel0518` | `poll plaidctl status` | polling wait | 2.244 s |
| startup | `mel0348` | `poll plaidctl status` | polling wait | 2.242 s |
| startup | `mel0429` | `poll plaidctl status` | polling wait | 2.241 s |
| startup | `mel0404` | `poll plaidctl status` | polling wait | 2.238 s |
| startup | `mel0159` | `poll plaidctl status` | polling wait | 2.237 s |
| startup | `mel0144` | `poll plaidctl status` | polling wait | 2.228 s |
| startup | `mel0417` | `poll plaidctl status` | polling wait | 2.228 s |
| startup | `mel0336` | `poll plaidctl status` | polling wait | 2.220 s |
| startup | `mel0134` | `poll plaidctl status` | polling wait | 2.219 s |
| startup | `mel0099` | `poll plaidctl status` | polling wait | 2.218 s |
| startup | `mel0386` | `poll plaidctl status` | polling wait | 2.216 s |
| shutdown | `mel0230` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.202 s |
| startup | `mel0417` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.198 s |
| startup | `mel0159` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.186 s |
| startup | `mel0134` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.181 s |
| startup | `mel0336` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.147 s |
| shutdown | `mel0518` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.147 s |
| startup | `mel0386` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.143 s |
| startup | `mel0099` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.132 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.116 s |
| startup | `mel0417` | `apptainer instance run ...` | command | 2.108 s |
| shutdown | `mel0348` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.093 s |
| startup | `mel0422` | `apptainer instance run ...` | command | 2.092 s |
| shutdown | `mel0506` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.080 s |
| shutdown | `mel0336` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.075 s |
| startup | `mel0518` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.066 s |
| startup | `mel0220` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.065 s |
| startup | `mel0506` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.064 s |
| startup | `mel0348` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.049 s |
| startup | `mel0429` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.046 s |
| shutdown | `mel0244` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 2.039 s |
| startup | `mel0230` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.037 s |
| startup | `mel0099` | `poll for the controller-signed kubelet certificate` | polling wait | 2.019 s |
| startup | `mel0417` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.012 s |
| startup | `mel0144` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.012 s |
| startup | `mel0134` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.011 s |
| startup | `mel0057` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 2.009 s |
| startup | `mel0159` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.008 s |
| startup | `mel0099` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 2.004 s |
| startup | `mel0244` | `apptainer instance run ...` | command | 1.996 s |
| startup | `mel0244` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.992 s |
| startup | `mel0404` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.987 s |
| startup | `mel0533` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.978 s |
| startup | `mel0429` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.977 s |
| startup | `mel0348` | `apptainer instance run ...` | command | 1.974 s |
| startup | `mel0422` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.961 s |
| startup | `mel0404` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.959 s |
| startup | `mel0336` | `skiff_launch_run NODE 0 bash skiff.sh check ROLE` | aggregate command | 1.947 s |
| startup | `mel0533` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.943 s |
| startup | `mel0144` | `apptainer instance run ...` | command | 1.934 s |
| shutdown | `mel0422` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 1.921 s |
| startup | `mel0348` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.920 s |
| shutdown | `mel0386` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 1.911 s |
| startup | `mel0159` | `apptainer instance run ...` | command | 1.907 s |
| startup | `mel0506` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.904 s |
| startup | `mel0518` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.900 s |
| startup | `mel0506` | `apptainer instance run ...` | command | 1.894 s |
| startup | `mel0230` | `apptainer instance run ...` | command | 1.893 s |
| startup | `mel0134` | `apptainer instance run ...` | command | 1.892 s |
| startup | `mel0533` | `apptainer instance run ...` | command | 1.886 s |
| startup | `mel0386` | `apptainer instance run ...` | command | 1.872 s |
| startup | `mel0220` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.856 s |
| startup | `mel0132` | `apptainer instance run ...` | command | 1.845 s |
| startup | `mel0404` | `apptainer instance run ...` | command | 1.843 s |
| startup | `mel0220` | `apptainer instance run ...` | command | 1.840 s |
| startup | `mel0099` | `apptainer instance run ...` | command | 1.838 s |
| startup | `mel0518` | `apptainer instance run ...` | command | 1.837 s |
| startup | `mel0429` | `apptainer instance run ...` | command | 1.830 s |
| startup | `mel0336` | `apptainer instance run ...` | command | 1.823 s |
| startup | `mel0057` | `apptainer instance run ...` | command | 1.808 s |
| startup | `mel0244` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.807 s |
| startup | `mel0230` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.773 s |
| startup | `mel0132` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.773 s |
| startup | `mel0047` | `skiff_config_validate; skiff_check_node ROLE` | aggregate commands | 1.696 s |
| startup | `mel0422` | `poll k3s kubectl get service -n kube-system kube-dns` | polling wait | 1.668 s |
| startup | `mel0057` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.562 s |
| startup | `mel0386` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.284 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.272 s |
| startup | `mel0047` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 1.270 s |
| startup | `mel0348` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.187 s |
| startup | `mel0404` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 1.186 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.182 s |
| startup | `mel0506` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.171 s |
| startup | `mel0230` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.162 s |
| startup | `mel0099` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.141 s |
| startup | `mel0159` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.135 s |
| startup | `mel0244` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.133 s |
| startup | `mel0417` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.127 s |
| startup | `mel0144` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.126 s |
| startup | `mel0518` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.122 s |
| startup | `mel0404` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.115 s |
| startup | `mel0134` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.114 s |
| startup | `mel0533` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.111 s |
| startup | `mel0336` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.095 s |
| startup | `mel0422` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.084 s |
| startup | `mel0429` | `apptainer --version` | direct command (caller: `skiff_node_report`) | 1.076 s |
| startup | `mel0134` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 1.040 s |
| startup | `mel0144` | `poll for the controller-signed kubelet certificate` | polling wait | 1.018 s |
| startup | `mel0132` | `poll for the controller-signed kubelet certificate` | polling wait | 1.018 s |
| startup | `mel0404` | `poll for the controller-signed kubelet certificate` | polling wait | 1.018 s |
| startup | `mel0220` | `poll for the controller-signed kubelet certificate` | polling wait | 1.017 s |
| startup | `mel0057` | `poll for the controller-signed kubelet certificate` | polling wait | 1.017 s |
| startup | `mel0134` | `poll for the controller-signed kubelet certificate` | polling wait | 1.016 s |
| startup | `mel0047` | `poll for the controller-signed kubelet certificate` | polling wait | 1.015 s |
| startup | `mel0244` | `poll for the controller-signed kubelet certificate` | polling wait | 1.013 s |
| startup | `mel0230` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.997 s |
| startup | `mel0518` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.997 s |
| startup | `mel0057` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.962 s |
| shutdown | `mel0057` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 0.909 s |
| shutdown | `mel0132` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 0.899 s |
| startup | `mel0348` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.874 s |
| startup | `mel0336` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.866 s |
| shutdown | `mel0099` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 0.856 s |
| shutdown | `mel0134` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 0.846 s |
| shutdown | `mel0144` | `skiff_launch_run NODE 15 ... _skiff_sweep_bubble` | command | 0.837 s |
| startup | `mel0417` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.787 s |
| startup | `mel0506` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.712 s |
| startup | `mel0220` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.704 s |
| startup | `mel0244` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.688 s |
| startup | `mel0336` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.652 s |
| startup | `mel0099` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.637 s |
| startup | `mel0518` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.626 s |
| startup | `mel0144` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.618 s |
| startup | `mel0386` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.613 s |
| startup | `mel0099` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.611 s |
| startup | `mel0159` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.609 s |
| startup | `mel0386` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.604 s |
| startup | `mel0422` | `start plaidd` | command dispatch | 0.580 s |
| startup | `mel0506` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.553 s |
| startup | `mel0429` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.549 s |
| startup | `mel0533` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.545 s |
| startup | `mel0159` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.530 s |
| startup | `mel0422` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.516 s |
| startup | `mel0047` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.506 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl get nodes/daemonset/pods/events/version` | aggregate commands | 0.492 s |
| startup | `mel0429` | `verify Service addresses; start skifflet` | aggregate commands | 0.480 s |
| startup | `mel0417` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.475 s |
| startup | `mel0417` | `verify Service addresses; start skifflet` | aggregate commands | 0.474 s |
| startup | `mel0386` | `verify Service addresses; start skifflet` | aggregate commands | 0.471 s |
| startup | `mel0422` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.464 s |
| startup | `mel0336` | `verify Service addresses; start skifflet` | aggregate commands | 0.459 s |
| startup | `mel0230` | `verify Service addresses; start skifflet` | aggregate commands | 0.450 s |
| startup | `mel0533` | `verify Service addresses; start skifflet` | aggregate commands | 0.446 s |
| startup | `mel0230` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.445 s |
| startup | `mel0220` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.443 s |
| startup | `mel0047` | `verify Service addresses; start skifflet` | aggregate commands | 0.430 s |
| startup | `mel0159` | `verify Service addresses; start skifflet` | aggregate commands | 0.427 s |
| startup | `mel0506` | `verify Service addresses; start skifflet` | aggregate commands | 0.413 s |
| startup | `mel0518` | `verify Service addresses; start skifflet` | aggregate commands | 0.412 s |
| startup | `mel0404` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.395 s |
| startup | `mel0348` | `verify Service addresses; start skifflet` | aggregate commands | 0.391 s |
| startup | `mel0533` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.386 s |
| startup | `mel0057` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.383 s |
| startup | `mel0244` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.379 s |
| startup | `mel0429` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.378 s |
| startup | `mel0220` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.375 s |
| startup | `mel0057` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.370 s |
| startup | `mel0417` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.368 s |
| startup | `mel0533` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.367 s |
| startup | `mel0099` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.364 s |
| startup | `mel0159` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.364 s |
| startup | `mel0144` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.362 s |
| startup | `mel0230` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.362 s |
| startup | `mel0386` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.360 s |
| startup | `mel0144` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.360 s |
| startup | `mel0336` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.359 s |
| startup | `mel0422` | `verify Service addresses; start skifflet` | aggregate commands | 0.359 s |
| startup | `mel0518` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.358 s |
| startup | `mel0134` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.356 s |
| startup | `mel0422` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.355 s |
| startup | `mel0220` | `verify Service addresses; start skifflet` | aggregate commands | 0.353 s |
| startup | `mel0132` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.352 s |
| startup | `mel0047` | `skiff_launch_run NODE 0 bash skiff.sh network-probe` | command | 0.352 s |
| startup | `mel0348` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.349 s |
| startup | `mel0132` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.349 s |
| startup | `mel0404` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.348 s |
| startup | `mel0134` | `verify Service addresses; start skifflet` | aggregate commands | 0.348 s |
| startup | `mel0132` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.347 s |
| startup | `mel0404` | `poll ip link show tap0` | polling wait | 0.342 s |
| startup | `mel0506` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.342 s |
| startup | `mel0132` | `verify Service addresses; start skifflet` | aggregate commands | 0.340 s |
| startup | `mel0144` | `verify Service addresses; start skifflet` | aggregate commands | 0.338 s |
| startup | `mel0244` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.338 s |
| startup | `mel0518` | `poll ip link show tap0` | polling wait | 0.337 s |
| startup | `mel0057` | `verify Service addresses; start skifflet` | aggregate commands | 0.336 s |
| startup | `mel0099` | `verify Service addresses; start skifflet` | aggregate commands | 0.334 s |
| startup | `mel0047` | `skiff_launch_run NODE 0 bash skiff.sh network-probe CONTROLLER_IP` | command | 0.334 s |
| startup | `mel0404` | `verify Service addresses; start skifflet` | aggregate commands | 0.326 s |
| startup | `mel0244` | `verify Service addresses; start skifflet` | aggregate commands | 0.324 s |
| startup | `mel0134` | `generate the node key/config/CSR with openssl` | aggregate commands | 0.290 s |
| pod setup | `login03.meluxina.lxp.lu` | `kubectl apply -f timestamp-per-node.yaml` | command | 0.289 s |
| startup | `mel0429` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.277 s |
| startup | `mel0506` | `start plaidd` | command dispatch | 0.264 s |
| startup | `mel0230` | `start plaidd` | command dispatch | 0.248 s |
| startup | `mel0230` | `poll ip link show tap0` | polling wait | 0.242 s |
| startup | `mel0159` | `poll ip link show tap0` | polling wait | 0.241 s |
| startup | `mel0244` | `poll ip link show tap0` | polling wait | 0.237 s |
| startup | `mel0057` | `poll ip link show tap0` | polling wait | 0.237 s |
| startup | `mel0417` | `poll ip link show tap0` | polling wait | 0.237 s |
| startup | `mel0533` | `poll ip link show tap0` | polling wait | 0.236 s |
| startup | `mel0429` | `poll ip link show tap0` | polling wait | 0.235 s |
| startup | `mel0132` | `poll ip link show tap0` | polling wait | 0.235 s |
| startup | `mel0220` | `poll ip link show tap0` | polling wait | 0.235 s |
| startup | `mel0134` | `poll ip link show tap0` | polling wait | 0.234 s |
| startup | `mel0144` | `poll ip link show tap0` | polling wait | 0.234 s |
| startup | `mel0386` | `poll ip link show tap0` | polling wait | 0.233 s |
| startup | `mel0336` | `poll ip link show tap0` | polling wait | 0.233 s |
| startup | `mel0099` | `poll ip link show tap0` | polling wait | 0.232 s |
| startup | `mel0422` | `poll ip link show tap0` | polling wait | 0.232 s |
| startup | `mel0506` | `poll ip link show tap0` | polling wait | 0.231 s |
| startup | `mel0348` | `poll ip link show tap0` | polling wait | 0.231 s |
| startup | `mel0429` | `start plaidd` | command dispatch | 0.224 s |
| startup | `mel0518` | `start plaidd` | command dispatch | 0.211 s |
| startup | `mel0348` | `poll Kubernetes API Service with curl and cluster DNS with drill` | polling wait | 0.204 s |
| startup | `mel0417` | `start plaidd` | command dispatch | 0.200 s |
| startup | `mel0047` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.183 s |
| startup | `mel0533` | `start plaidd` | command dispatch | 0.182 s |
| startup | `mel0348` | `start plaidd` | command dispatch | 0.175 s |
| startup | `mel0047` | `start plaidd` | command dispatch | 0.153 s |
| startup | `mel0159` | `start plaidd` | command dispatch | 0.135 s |
| startup | `mel0386` | `start plaidd` | command dispatch | 0.126 s |
| startup | `mel0144` | `start plaidd` | command dispatch | 0.124 s |
| startup | `mel0134` | `start plaidd` | command dispatch | 0.122 s |
| startup | `mel0132` | `start plaidd` | command dispatch | 0.121 s |
| startup | `mel0336` | `start plaidd` | command dispatch | 0.116 s |
| startup | `mel0244` | `start plaidd` | command dispatch | 0.108 s |
| startup | `mel0057` | `start plaidd` | command dispatch | 0.105 s |
| startup | `mel0518` | `poll apptainer instance list for the instance PID` | polling wait | 0.101 s |
| startup | `mel0099` | `start plaidd` | command dispatch | 0.101 s |
| shutdown | `mel0047` | `skiff_launch_stop; wait for bubble processes` | aggregate wait | 0.096 s |
| startup | `mel0404` | `start plaidd` | command dispatch | 0.092 s |
| startup | `mel0047` | `poll ip link show tap0` | polling wait | 0.085 s |
| startup | `mel0506` | `poll apptainer instance list for the instance PID` | polling wait | 0.080 s |
| startup | `mel0220` | `start plaidd` | command dispatch | 0.078 s |
| startup | `mel0134` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.076 s |
| startup | `mel0220` | `poll apptainer instance list for the instance PID` | polling wait | 0.060 s |
| startup | `mel0099` | `poll apptainer instance list for the instance PID` | polling wait | 0.060 s |
| startup | `mel0386` | `poll apptainer instance list for the instance PID` | polling wait | 0.057 s |
| startup | `mel0132` | `poll apptainer instance list for the instance PID` | polling wait | 0.056 s |
| startup | `mel0404` | `poll apptainer instance list for the instance PID` | polling wait | 0.055 s |
| startup | `mel0134` | `poll apptainer instance list for the instance PID` | polling wait | 0.051 s |
| startup | `mel0159` | `poll apptainer instance list for the instance PID` | polling wait | 0.050 s |
| startup | `mel0230` | `poll apptainer instance list for the instance PID` | polling wait | 0.049 s |
| startup | `mel0244` | `poll apptainer instance list for the instance PID` | polling wait | 0.049 s |
| startup | `mel0047` | `poll apptainer instance list for the instance PID` | polling wait | 0.049 s |
| startup | `mel0533` | `poll apptainer instance list for the instance PID` | polling wait | 0.048 s |
| startup | `mel0144` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.048 s |
| startup | `mel0057` | `poll apptainer instance list for the instance PID` | polling wait | 0.048 s |
| startup | `mel0429` | `poll apptainer instance list for the instance PID` | polling wait | 0.048 s |
| startup | `mel0417` | `poll apptainer instance list for the instance PID` | polling wait | 0.047 s |
| startup | `mel0336` | `poll apptainer instance list for the instance PID` | polling wait | 0.047 s |
| startup | `mel0348` | `poll apptainer instance list for the instance PID` | polling wait | 0.046 s |
| startup | `mel0422` | `poll apptainer instance list for the instance PID` | polling wait | 0.046 s |
| startup | `mel0144` | `poll apptainer instance list for the instance PID` | polling wait | 0.045 s |
| startup | `mel0057` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.045 s |
| startup | `mel0533` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.045 s |
| startup | `mel0099` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.044 s |
| startup | `mel0230` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.044 s |
| startup | `mel0057` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.041 s |
| startup | `mel0518` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.040 s |
| startup | `mel0336` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.039 s |
| startup | `mel0417` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.038 s |
| startup | `mel0099` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.037 s |
| startup | `mel0144` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.037 s |
| startup | `mel0429` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.036 s |
| shutdown | `login03.meluxina.lxp.lu` | `scancel --batch --signal=TERM JOB_ID` | command | 0.036 s |
| startup | `mel0244` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.035 s |
| startup | `mel0220` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.033 s |
| startup | `mel0159` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.033 s |
| startup | `mel0057` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.033 s |
| startup | `mel0404` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.032 s |
| startup | `mel0429` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.032 s |
| startup | `mel0422` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.031 s |
| startup | `mel0244` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.031 s |
| startup | `mel0134` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.030 s |
| startup | `mel0047` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.030 s |
| startup | `mel0533` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.029 s |
| startup | `mel0422` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.029 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.029 s |
| startup | `mel0047` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.028 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.028 s |
| startup | `mel0386` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.028 s |
| startup | `mel0159` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.027 s |
| startup | `mel0417` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.027 s |
| startup | `mel0336` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.027 s |
| startup | `mel0230` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.026 s |
| startup | `mel0386` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.026 s |
| startup | `mel0132` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.026 s |
| startup | `mel0506` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.026 s |
| startup | `mel0404` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.026 s |
| startup | `mel0348` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.024 s |
| startup | `mel0518` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.023 s |
| startup | `mel0506` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.023 s |
| startup | `mel0386` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.023 s |
| startup | `mel0348` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.022 s |
| startup | `mel0132` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.022 s |
| startup | `mel0057` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.019 s |
| startup | `mel0348` | `apptainer --version` | direct command (caller: `skiff_check_node`) | 0.018 s |
| startup | `mel0244` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.018 s |
| startup | `mel0336` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.016 s |
| startup | `mel0429` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.016 s |
| startup | `mel0144` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.015 s |
| startup | `mel0134` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.014 s |
| startup | `mel0417` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.014 s |
| startup | `mel0533` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.014 s |
| startup | `mel0506` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.013 s |
| startup | `mel0230` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.013 s |
| startup | `mel0099` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0159` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0404` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0220` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0422` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0518` | `configure slirp4netns forwards; start socat relays` | aggregate commands | 0.012 s |
| startup | `mel0417` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.009 s |
| startup | `mel0047` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.009 s |
| startup | `mel0159` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0220` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0047` | `skiff_image_files; build a newer/missing SIF with apptainer build` | command/branch | 0.008 s |
| startup | `mel0422` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.008 s |
| startup | `mel0506` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.008 s |
| startup | `mel0417` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0132` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0518` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0404` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0159` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.008 s |
| startup | `mel0230` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0057` | `start background srun for worker bubble` | command dispatch | 0.008 s |
| startup | `mel0518` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0348` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0244` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0386` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0244` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0422` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0404` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0386` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0533` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0134` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0099` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0336` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0099` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0533` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0134` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0230` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0144` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0429` | `slirp4netns --version` | direct command (caller: `skiff_node_report`) | 0.007 s |
| startup | `mel0506` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0348` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0336` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0144` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0429` | `start background srun for worker bubble` | command dispatch | 0.007 s |
| startup | `mel0533` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0134` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0047` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0336` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0386` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0220` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.003 s |
| startup | `mel0417` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0518` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0144` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0244` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0230` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0429` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0159` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0506` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0099` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0057` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0404` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0348` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0132` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |
| startup | `mel0422` | `slirp4netns --version` | direct command (caller: `skiff_check_node`) | 0.002 s |

## Per-node bubble entrypoint

| Host | Bubble | Role | Bootstrap | In-bubble cleanup |
| --- | ---: | --- | ---: | ---: |
| `mel0047` | 1 | controller | 19.853 s | — |
| `mel0057` | 2 | node | 7.393 s | — |
| `mel0099` | 3 | node | 8.418 s | — |
| `mel0132` | 4 | node | 6.570 s | — |
| `mel0134` | 5 | node | 7.579 s | — |
| `mel0144` | 6 | node | 7.330 s | — |
| `mel0159` | 7 | node | 9.518 s | — |
| `mel0220` | 8 | node | 7.044 s | — |
| `mel0230` | 9 | node | 9.555 s | — |
| `mel0244` | 10 | node | 6.874 s | — |
| `mel0336` | 11 | node | 9.850 s | — |
| `mel0348` | 12 | node | 9.169 s | — |
| `mel0386` | 13 | node | 9.524 s | — |
| `mel0404` | 14 | node | 7.651 s | — |
| `mel0417` | 15 | node | 9.602 s | — |
| `mel0422` | 16 | node | 9.301 s | — |
| `mel0429` | 17 | node | 9.184 s | — |
| `mel0506` | 18 | node | 9.553 s | — |
| `mel0518` | 19 | node | 9.215 s | — |
| `mel0533` | 20 | node | 9.114 s | — |

## Timestamp pods

| Pod | Node | Printed marker |
| --- | --- | --- |
| `timestamp-per-node-6r2f2` | `mel0047` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0047 pod=timestamp-per-node-6r2f2` |
| `timestamp-per-node-8845r` | `mel0057` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0057 pod=timestamp-per-node-8845r` |
| `timestamp-per-node-j6pbr` | `mel0099` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0099 pod=timestamp-per-node-j6pbr` |
| `timestamp-per-node-6nptm` | `mel0132` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0132 pod=timestamp-per-node-6nptm` |
| `timestamp-per-node-wxtmg` | `mel0134` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0134 pod=timestamp-per-node-wxtmg` |
| `timestamp-per-node-q9xhd` | `mel0144` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0144 pod=timestamp-per-node-q9xhd` |
| `timestamp-per-node-v4jr7` | `mel0159` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0159 pod=timestamp-per-node-v4jr7` |
| `timestamp-per-node-lrn44` | `mel0220` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0220 pod=timestamp-per-node-lrn44` |
| `timestamp-per-node-mw9nb` | `mel0230` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0230 pod=timestamp-per-node-mw9nb` |
| `timestamp-per-node-k6hm7` | `mel0244` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0244 pod=timestamp-per-node-k6hm7` |
| `timestamp-per-node-c9lwm` | `mel0336` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0336 pod=timestamp-per-node-c9lwm` |
| `timestamp-per-node-cmx6q` | `mel0348` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0348 pod=timestamp-per-node-cmx6q` |
| `timestamp-per-node-djmbc` | `mel0386` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0386 pod=timestamp-per-node-djmbc` |
| `timestamp-per-node-bdjkd` | `mel0404` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0404 pod=timestamp-per-node-bdjkd` |
| `timestamp-per-node-49td8` | `mel0417` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0417 pod=timestamp-per-node-49td8` |
| `timestamp-per-node-hn5gx` | `mel0422` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0422 pod=timestamp-per-node-hn5gx` |
| `timestamp-per-node-7h4nw` | `mel0429` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0429 pod=timestamp-per-node-7h4nw` |
| `timestamp-per-node-6m6hw` | `mel0506` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0506 pod=timestamp-per-node-6m6hw` |
| `timestamp-per-node-gr7st` | `mel0518` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0518 pod=timestamp-per-node-gr7st` |
| `timestamp-per-node-h45fw` | `mel0533` | `SKIFF_LIFECYCLE_POD epoch_seconds=1793359170 utc=2026-10-30T11:19:30Z node=mel0533 pod=timestamp-per-node-h45fw` |

## Interpretation

The complete, time-sorted event stream is in `timeline.tsv`. It retains every
Skiff supervisor, bubble and entrypoint event plus Slurm allocation and
Kubernetes Pod transitions. Per-process raw files remain in `skiff-timings/`.

`intervals.tsv` subtracts consecutive events within each process or Pod so
individual setup and cleanup steps can be compared without using the global
interleaving of concurrent nodes.

Cross-host intervals use UTC epoch timestamps and therefore require synchronized
node clocks. Slurm and Kubernetes timestamps have one-second precision; Skiff and
observer shell events record nanoseconds. The run enables `SKIFF_TIMING=1`, so the
reported values include the small cost of clock reads and shared-storage appends.
This benchmark measures a warm system-image path unless the cache is deliberately
cleared and that cache state is recorded separately.
