package provider

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	PodHandler "skifflet/internal/compute/podhandler"
)

// What happens when the API changes the spec of a pod Skifflet is running.
//
// Labels and annotations are applied (the lifecycle owner follows them, nothing
// restarts). Changes that cannot be applied to a container that is running (its
// image, its resources, a pod deadline, ephemeral containers) are reported: the
// running pod is left exactly as it is, its status keeps showing what really runs
// (the image and attempt of each container), and a Warning event says what was
// asked, what runs, and what to do instead.
//
// The provider must answer an update with success or with an error, and both have a
// consequence in Virtual Kubelet's controller: an error makes it declare the pod
// Pending or Failed, which would falsely condemn a healthy pod, while a success
// makes it record that the pod was "updated successfully" at once. So the update
// succeeds, and the misleading success event of an update that changed nothing is
// dropped by the recorder the controller is given (NewUpdateEventFilter).
// Virtual Kubelet compares the running pod with the API's on every change of the
// pod, so an unapplied change is met again and again: it is reported once each time
// it differs from the last thing reported.

const providerUpdateSuccess = "ProviderUpdateSuccess" // the reason of Virtual Kubelet's event

// reportUnapplied notes what an update could not apply.
func (v *VirtualK8S) reportUnapplied(owner *PodHandler.PodLifecycle, key client.ObjectKey, changes []string) {
	v.suppressSuccess.Store(key, struct{}{})
	fingerprint := strings.Join(changes, "\n")
	if last, ok := v.lastUnapplied.Load(key); ok && last == fingerprint {
		return
	}
	v.lastUnapplied.Store(key, fingerprint)
	for _, change := range changes {
		v.Logger.Info("pod update not applied", "obj", key, "reason", change)
		owner.PodEvent(corev1.EventTypeWarning, "UpdateNotApplied", "%s", change)
	}
}

// clearUnapplied forgets what was reported for a pod, when it is gone.
func (v *VirtualK8S) clearUnapplied(key client.ObjectKey) {
	v.lastUnapplied.Delete(key)
	v.suppressSuccess.Delete(key)
}

// takeSuppression says whether the success event of the update just made is to be
// dropped, and forgets it.
func (v *VirtualK8S) takeSuppression(key client.ObjectKey) bool {
	_, suppressed := v.suppressSuccess.LoadAndDelete(key)
	return suppressed
}

// NewUpdateEventFilter wraps the recorder given to Virtual Kubelet's pod controller
// so that its "updated successfully" event is not recorded for an update that
// changed nothing of what was asked.
func NewUpdateEventFilter(inner record.EventRecorder, v *VirtualK8S) record.EventRecorder {
	return &updateEventFilter{EventRecorder: inner, v: v}
}

type updateEventFilter struct {
	record.EventRecorder
	v *VirtualK8S
}

func (f *updateEventFilter) dropped(object runtime.Object, reason string) bool {
	pod, ok := object.(*corev1.Pod)
	return ok && reason == providerUpdateSuccess && f.v.takeSuppression(client.ObjectKeyFromObject(pod))
}

func (f *updateEventFilter) Event(object runtime.Object, eventtype, reason, message string) {
	if !f.dropped(object, reason) {
		f.EventRecorder.Event(object, eventtype, reason, message)
	}
}

func (f *updateEventFilter) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if !f.dropped(object, reason) {
		f.EventRecorder.Eventf(object, eventtype, reason, messageFmt, args...)
	}
}

func (f *updateEventFilter) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	if !f.dropped(object, reason) {
		f.EventRecorder.AnnotatedEventf(object, annotations, eventtype, reason, messageFmt, args...)
	}
}
