package direct

import (
	"os"
	"path/filepath"
	"testing"
)

func kubeconfigWith(t *testing.T, user string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	yaml := "apiVersion: v1\nkind: Config\ncurrent-context: c\n" +
		"clusters: [{name: k, cluster: {server: 'https://127.0.0.1:6443'}}]\n" +
		"users: [{name: u, user: " + user + "}]\n" +
		"contexts: [{name: c, context: {cluster: k, user: u}}]\n"
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The key is the same wherever the same kubeconfig is read, differs between clusters, and
// is not the private key itself.
func TestKeyFromKubeconfig(t *testing.T) {
	a := kubeconfigWith(t, "{client-key-data: Y2x1c3RlciBBIGtleQ==}") // "cluster A key"
	b := kubeconfigWith(t, "{client-key-data: Y2x1c3RlciBCIGtleQ==}")
	k1, err := KeyFromKubeconfig(a)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := KeyFromKubeconfig(a)
	k3, _ := KeyFromKubeconfig(b)
	if string(k1) != string(k2) || string(k1) == string(k3) || len(k1) != 32 || string(k1) == "cluster A key" {
		t.Errorf("keys %x %x %x", k1, k2, k3)
	}
	if string(k1) != string(DeriveKey([]byte("cluster A key"))) {
		t.Error("the key is not the derivation of the private key")
	}
	if _, err := KeyFromKubeconfig(kubeconfigWith(t, "{token: abc}")); err == nil {
		t.Error("a user with no private key gave a key")
	}
	if _, err := KeyFromKubeconfig(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a kubeconfig that is not there gave a key")
	}
}
