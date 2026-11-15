package provider

import (
	"sync/atomic"

	"skifflet/internal/compute"
	"skifflet/internal/compute/image"
)

// unpackedImagesOwned says that this Skifflet holds the lock of the node's scratch directory
// (see AcquireInstanceLock), and so that the images unpacked there (see image.Image.Unpacked)
// are its own: until then they may be those of the Skifflet that does hold it.
var unpackedImagesOwned atomic.Bool

// claimUnpackedImages is for the Skifflet that has just got the node's lock: the directory of
// the node's unpacked images is its own from here on, and it empties it. An earlier Skifflet
// that was killed may have left its copies there, which take room on the node and which this
// one would not use (see image.Image.Unpacked). No container of this node runs yet, so nothing
// reads them.
func claimUnpackedImages() error {
	unpackedImagesOwned.Store(true)
	return image.RemoveUnpacked(compute.Skiff.UnpackedImagesDir())
}

// RemoveUnpackedImages removes the images this Skifflet unpacked for its node. It is for when
// the node's pods have stopped (podhandler.PodRegistry.DrainAll) and nothing reads them any
// more: the copies are as large as the images, on a scratch directory that may be held in
// memory, and the node is not left with them. A Skifflet that never got the node's lock
// removes nothing.
func RemoveUnpackedImages() error {
	if !unpackedImagesOwned.Load() {
		return nil
	}
	return image.RemoveUnpacked(compute.Skiff.UnpackedImagesDir())
}
