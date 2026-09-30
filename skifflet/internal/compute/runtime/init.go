package runtime

import (
	"fmt"
	"os"

	"skifflet/internal/compute"
	"skifflet/internal/compute/endpoint"
	"skifflet/internal/compute/image"
)

func Initialize(pauseImage string) error {
	if compute.Environment.PodsDirectory != "" {
		compute.Skiff = endpoint.SkiffWithPods(compute.Environment.WorkingDirectory, compute.Environment.PodsDirectory)
	} else {
		compute.Skiff = endpoint.Skiff(compute.Environment.WorkingDirectory)
	}

	// create the ~/.skiff directory, if it does not exist.
	if err := os.MkdirAll(compute.Skiff.String(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("Failed to create RuntimeDir '%s': %w", compute.Skiff.String(), err)
	}

	// create the ~/.skiff/image directory, if it does not exist.
	if err := os.MkdirAll(compute.Skiff.ImageDir(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("Failed to create ImageDir '%s': %w", compute.Skiff.ImageDir(), err)
	}

	// create the /tmp/.skiff/.pods directory, if it does not exist.
	if err := os.MkdirAll(compute.Skiff.PodsDir(), endpoint.PodGlobalDirectoryPermissions); err != nil {
		return fmt.Errorf("Failed to create PodsDir '%s': %w", compute.Skiff.PodsDir(), err)
	}

	// Sweep leftover temp image files from prior crashed pulls
	if err := image.CleanupTempFiles(compute.Skiff.ImageDir()); err != nil {
		compute.DefaultLogger.Error(err, "failed to cleanup temp image files")
	}

	if pauseImage != "" {
		if _, err := image.Pull(compute.Skiff.ImageDir(), image.Docker, pauseImage); err != nil {
			compute.DefaultLogger.Info("Preflight pause image warmup skipped or deferred", "image", pauseImage, "err", err)
		}
	}

	compute.DefaultLogger.Info("Runtime info",
		"WorkingDirectory", compute.Skiff.String(),
		"PodsDirectory", compute.Skiff.PodsDir(),
		"PlaidSocket", compute.Environment.PlaidSocketPath(),
		"ClusterDomain", compute.Environment.ClusterDomainOrDefault(),
	)

	return nil
}
