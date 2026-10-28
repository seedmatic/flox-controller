package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
)

func envWith(status metav1.ConditionStatus, reason string, annotations map[string]string) *floxv1alpha1.FloxEnv {
	env := &floxv1alpha1.FloxEnv{ObjectMeta: metav1.ObjectMeta{Name: "kube", Namespace: "rke2lab-system", Annotations: annotations}}
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: reason})
	return env
}

func TestAnEnvAnnotationOrStatusNoiseDoesNotEnqueueTheCatalog(t *testing.T) {
	before := envWith(metav1.ConditionFalse, "Realizing", nil)
	after := envWith(metav1.ConditionFalse, "Realizing", map[string]string{relockAnnotation: "sha1:abc"})
	after.Status.RelockToken = "sha1:abc"
	if envReadinessChange().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("an annotation-only / same-readiness FloxEnv update enqueued the catalog")
	}
}

func TestAnEnvReadinessMoveEnqueuesTheCatalog(t *testing.T) {
	before := envWith(metav1.ConditionFalse, "Realizing", nil)
	after := envWith(metav1.ConditionTrue, "Realized", nil)
	if !envReadinessChange().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("a Ready Realizing -> Realized move did not enqueue the catalog")
	}
	if !envReadinessChange().Create(event.CreateEvent{Object: after}) {
		t.Fatal("a new FloxEnv did not enqueue the catalog")
	}
}

func TestTheCatalogsOwnStatusWriteDoesNotEnqueueIt(t *testing.T) {
	before := &floxv1alpha1.FloxCatalog{ObjectMeta: metav1.ObjectMeta{Name: "catalog", Generation: 1}}
	after := before.DeepCopy()
	after.Status.Revision = "develop@sha1:edb9db1c"
	if catalogChange().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("a status-only FloxCatalog update re-enqueued it")
	}
	spec := before.DeepCopy()
	spec.Generation = 2
	if !catalogChange().Update(event.UpdateEvent{ObjectOld: before, ObjectNew: spec}) {
		t.Fatal("a spec change (generation bump) did not enqueue the catalog")
	}
}
