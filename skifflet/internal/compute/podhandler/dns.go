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

// PrepareDNS generates resolv.conf and hosts in the pod's job directory, as the kubelet
// does. A pod that uses the cluster DNS searches its namespace, then the cluster, in the
// cluster's domain; one with dnsPolicy Default (CoreDNS among them) has the bubble's
// resolver; dnsConfig adds to either (or is all there is, with None), merged as the kubelet
// merges it. The hosts file names the pod (by its fully qualified name too, when it has a
// subdomain) and its hostAliases.
func PrepareDNS(pod *corev1.Pod, podDir endpoint.PodPath, kubeDNSIP, clusterDomain, podIP string) error {
	resolvConfContent, err := resolvConf(pod, kubeDNSIP, clusterDomain)
	if err != nil {
		return err
	}

	jobDir := podDir.JobDir()
	if err := os.MkdirAll(jobDir, endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("failed to create job dir for DNS: %w", err)
	}

	resolvPath := podDir.ResolvConf()
	if err := os.WriteFile(resolvPath, []byte(resolvConfContent), 0644); err != nil {
		return fmt.Errorf("error writing resolv.conf: %w", err)
	}

	hostsPath := podDir.HostsFile()
	if err := os.WriteFile(hostsPath, []byte(hostsFile(pod, podIP, clusterDomain)), 0644); err != nil {
		return fmt.Errorf("error writing hosts: %w", err)
	}

	return nil
}

// dnsSettings is a resolv.conf: the name servers, the search path and the options.
type dnsSettings struct {
	nameservers, searches, options []string
}

// maxDNSSearches is how many search domains the kubelet keeps.
const maxDNSSearches = 32

// resolvConf is the pod's resolv.conf.
func resolvConf(pod *corev1.Pod, kubeDNSIP, clusterDomain string) (string, error) {
	var base dnsSettings
	switch pod.Spec.DNSPolicy {
	case corev1.DNSDefault:
		host := getHostResolvConf(kubeDNSIP, "/etc/resolv.conf")
		if !hasNameserver(host) {
			return "", fmt.Errorf("no usable upstream nameserver in bubble resolv.conf")
		}
		base = parseResolvConf(host)
	case corev1.DNSNone:
	default:
		base = dnsSettings{
			nameservers: []string{kubeDNSIP},
			searches:    []string{pod.Namespace + ".svc." + clusterDomain, "svc." + clusterDomain, clusterDomain},
			options:     []string{"ndots:5"},
		}
	}
	if c := pod.Spec.DNSConfig; c != nil {
		base.nameservers = appendUnique(base.nameservers, c.Nameservers...)
		base.searches = appendUnique(base.searches, c.Searches...)
		for _, opt := range c.Options {
			o := opt.Name
			if opt.Value != nil {
				o += ":" + *opt.Value
			}
			base.options = mergeOption(base.options, o)
		}
	}
	if len(base.searches) > maxDNSSearches {
		base.searches = base.searches[:maxDNSSearches]
	}
	var b strings.Builder
	if len(base.searches) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(base.searches, " "))
	}
	for _, ns := range base.nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	if len(base.options) > 0 {
		fmt.Fprintf(&b, "options %s\n", strings.Join(base.options, " "))
	}
	return b.String(), nil
}

// parseResolvConf reads the name servers, the search path (search or domain) and the
// options of a resolv.conf; the last search or domain line is the one that counts.
func parseResolvConf(content string) dnsSettings {
	var s dnsSettings
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			s.nameservers = appendUnique(s.nameservers, fields[1])
		case "search", "domain":
			s.searches = appendUnique(nil, fields[1:]...)
		case "options":
			for _, o := range fields[1:] {
				s.options = mergeOption(s.options, o)
			}
		}
	}
	return s
}

func appendUnique(list []string, items ...string) []string {
	for _, item := range items {
		found := false
		for _, have := range list {
			if have == item {
				found = true
				break
			}
		}
		if !found {
			list = append(list, item)
		}
	}
	return list
}

// mergeOption adds a resolver option, replacing one of the same name (ndots:2 replaces ndots:5).
func mergeOption(options []string, option string) []string {
	name, _, _ := strings.Cut(option, ":")
	for i, have := range options {
		if haveName, _, _ := strings.Cut(have, ":"); haveName == name {
			options[i] = option
			return options
		}
	}
	return append(options, option)
}

// maxHostnameLength is the longest hostname the kubelet gives a pod; a longer pod name is cut.
const maxHostnameLength = 63

// podHostname is the pod's hostname and, when it has a subdomain, its fully qualified name
// (<hostname>.<subdomain>.<namespace>.svc.<domain>), as the kubelet makes them. With
// setHostnameAsFQDN the containers' hostname is the fully qualified name, which may then
// have at most 64 characters.
func podHostname(pod *corev1.Pod, clusterDomain string) (hostname, fqdn string, err error) {
	hostname = pod.Spec.Hostname
	if hostname == "" {
		hostname = pod.Name
		if len(hostname) > maxHostnameLength {
			hostname = strings.TrimRight(hostname[:maxHostnameLength], "-.")
		}
	}
	if pod.Spec.Subdomain != "" {
		fqdn = fmt.Sprintf("%s.%s.%s.svc.%s", hostname, pod.Spec.Subdomain, pod.Namespace, clusterDomain)
	}
	if pod.Spec.SetHostnameAsFQDN != nil && *pod.Spec.SetHostnameAsFQDN && fqdn != "" && len(fqdn) > maxHostnameLength+1 {
		return "", "", &ConfigError{Message: fmt.Sprintf("failed to construct FQDN from pod hostname and cluster domain, FQDN %s is too long (64 characters is the max, %d characters requested)", fqdn, len(fqdn)), Reason: "CreateContainerConfigError"}
	}
	return hostname, fqdn, nil
}

// containerHostname is the hostname the pod's containers have.
func containerHostname(pod *corev1.Pod, clusterDomain string) (string, error) {
	hostname, fqdn, err := podHostname(pod, clusterDomain)
	if err != nil {
		return "", err
	}
	if pod.Spec.SetHostnameAsFQDN != nil && *pod.Spec.SetHostnameAsFQDN && fqdn != "" {
		return fqdn, nil
	}
	return hostname, nil
}

// hostsFile is the pod's /etc/hosts, in the kubelet's form.
func hostsFile(pod *corev1.Pod, podIP, clusterDomain string) string {
	var b strings.Builder
	b.WriteString("# Kubernetes-managed hosts file.\n")
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	b.WriteString("fe00::0\tip6-localnet\n")
	b.WriteString("fe00::0\tip6-mcastprefix\n")
	b.WriteString("fe00::1\tip6-allnodes\n")
	b.WriteString("fe00::2\tip6-allrouters\n")
	hostname, fqdn, _ := podHostname(pod, clusterDomain) // (a name that is too long fails the container, not the file)
	if podIP != "" {
		if fqdn != "" {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", podIP, fqdn, hostname)
		} else {
			fmt.Fprintf(&b, "%s\t%s\n", podIP, hostname)
		}
	}
	if len(pod.Spec.HostAliases) > 0 {
		b.WriteString("\n# Entries added by HostAliases.\n")
		for _, alias := range pod.Spec.HostAliases {
			fmt.Fprintf(&b, "%s\t%s\n", alias.IP, strings.Join(alias.Hostnames, "\t"))
		}
	}
	return b.String()
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
