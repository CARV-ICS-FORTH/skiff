package podhandler

import (
	"fmt"
	"net"
	"os"
	"strings"

	"skifflet/internal/compute/endpoint"

	corev1 "k8s.io/api/core/v1"
)

func getHostResolvConf(kubeDNSIP string, resolvConfPath string) string {
	if resolvConfPath == "" {
		resolvConfPath = "/etc/resolv.conf"
	}

	paths := []string{resolvConfPath}
	if resolvConfPath == "/etc/resolv.conf" {
		paths = append(paths, "/run/systemd/resolve/resolv.conf")
	}
	var raw string
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			content := string(b)
			if strings.Contains(content, "127.0.0.53") && p == "/etc/resolv.conf" {
				if sysb, sysErr := os.ReadFile("/run/systemd/resolve/resolv.conf"); sysErr == nil && len(strings.TrimSpace(string(sysb))) > 0 {
					content = string(sysb)
				}
			}
			if len(strings.TrimSpace(content)) > 0 {
				raw = content
				break
			}
		}
	}

	if raw == "" {
		return ""
	}

	var validLines []string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "nameserver") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 2 {
				ipStr := parts[1]
				ip := net.ParseIP(ipStr)
				if ip != nil && (ip.IsLoopback() || ipStr == kubeDNSIP) {
					continue
				}
			}
		}
		validLines = append(validLines, line)
	}

	return strings.Join(validLines, "\n") + "\n"
}

// PrepareDNS generates resolv.conf and hosts in the pod's job directory. A pod
// that uses the cluster DNS searches its namespace, then the cluster, in the
// cluster's domain.
func PrepareDNS(pod *corev1.Pod, podDir endpoint.PodPath, kubeDNSIP, clusterDomain, podIP string) error {
	var resolvConfContent string
	isCoreDNS := strings.HasPrefix(pod.Name, "coredns") || (pod.Labels != nil && pod.Labels["k8s-app"] == "kube-dns")

	if pod.Spec.DNSPolicy == corev1.DNSDefault || isCoreDNS {
		resolvConfContent = getHostResolvConf(kubeDNSIP, "/etc/resolv.conf")
		if !hasNameserver(resolvConfContent) {
			return fmt.Errorf("no usable upstream nameserver in bubble resolv.conf")
		}
	} else if pod.Spec.DNSPolicy == corev1.DNSNone {
		resolvConfContent = ""
	} else {
		resolvConfContent = fmt.Sprintf(
			"search %s.svc.%s svc.%s %s\nnameserver %s\noptions ndots:5\n",
			pod.Namespace, clusterDomain, clusterDomain, clusterDomain, kubeDNSIP,
		)
	}

	if pod.Spec.DNSConfig != nil {
		var lines []string
		if resolvConfContent != "" {
			lines = append(lines, strings.TrimSpace(resolvConfContent))
		}
		if len(pod.Spec.DNSConfig.Searches) > 0 {
			lines = append(lines, fmt.Sprintf("search %s", strings.Join(pod.Spec.DNSConfig.Searches, " ")))
		}
		for _, ns := range pod.Spec.DNSConfig.Nameservers {
			lines = append(lines, fmt.Sprintf("nameserver %s", ns))
		}
		for _, opt := range pod.Spec.DNSConfig.Options {
			if opt.Value != nil {
				lines = append(lines, fmt.Sprintf("options %s:%s", opt.Name, *opt.Value))
			} else {
				lines = append(lines, fmt.Sprintf("options %s", opt.Name))
			}
		}
		resolvConfContent = strings.Join(lines, "\n") + "\n"
	}

	jobDir := podDir.JobDir()
	if err := os.MkdirAll(jobDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("failed to create job dir for DNS: %w", err)
	}

	resolvPath := podDir.ResolvConf()
	if err := os.WriteFile(resolvPath, []byte(resolvConfContent), 0644); err != nil {
		return fmt.Errorf("error writing resolv.conf: %w", err)
	}

	hostname := pod.Name
	if pod.Spec.Hostname != "" {
		hostname = pod.Spec.Hostname
	}
	hostsContent := fmt.Sprintf("127.0.0.1 localhost\n%s %s\n", podIP, hostname)
	hostsPath := podDir.HostsFile()
	if err := os.WriteFile(hostsPath, []byte(hostsContent), 0644); err != nil {
		return fmt.Errorf("error writing hosts: %w", err)
	}

	return nil
}

func hasNameserver(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[0] == "nameserver" && net.ParseIP(parts[1]) != nil {
			return true
		}
	}
	return false
}
