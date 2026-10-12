package provider

import (
	"k8s.io/apimachinery/pkg/types"

	PodHandler "skifflet/internal/compute/podhandler"
)

// forgetOwner takes a test's pod out of the registry.
func forgetOwner(uid types.UID) {
	if owner := PodHandler.GlobalRegistry.Get(uid); owner != nil {
		PodHandler.GlobalRegistry.DeleteOwner(owner)
	}
}
