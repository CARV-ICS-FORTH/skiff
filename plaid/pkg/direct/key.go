package direct

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
)

// keyLabel separates the key of the direct path from any other use of the cluster's key.
const keyLabel = "skiff direct path v2"

// DeriveKey is the key the preambles and acks are authenticated with: the HMAC-SHA256 of a
// fixed label under the private key of the cluster's kubeconfig. Every node of a cluster
// reads the same kubeconfig (Skiff keeps it with the cluster's state, mode 0600), so every
// node derives the same key; a pod cannot read it unless it mounts that directory, in
// which case it holds the cluster's administrator credentials anyway.
func DeriveKey(clusterKey []byte) []byte {
	h := hmac.New(sha256.New, clusterKey)
	h.Write([]byte(keyLabel))
	return h.Sum(nil)
}

// KeyFromKubeconfig reads the private key of the current context's user from the
// kubeconfig at path and derives the key of the direct path from it.
func KeyFromKubeconfig(path string) ([]byte, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	ctx := cfg.Contexts[cfg.CurrentContext]
	if ctx == nil {
		return nil, fmt.Errorf("%s has no current context", path)
	}
	user := cfg.AuthInfos[ctx.AuthInfo]
	if user == nil || len(user.ClientKeyData) == 0 {
		return nil, errors.New(path + " has no client-key-data for its current user")
	}
	return DeriveKey(user.ClientKeyData), nil
}
