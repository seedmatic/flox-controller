package controller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

// blockingProvisioner holds every Realize until its context ends or release closes, and counts how
// many run at once.
type blockingProvisioner struct {
	release chan struct{}
	running atomic.Int32
	peak    atomic.Int32
}

func (b *blockingProvisioner) Realize(ctx context.Context, _ provisioner.RealizeRequest) (provisioner.RealizeResult, error) {
	n := b.running.Add(1)
	defer b.running.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return provisioner.RealizeResult{}, ctx.Err()
	case <-b.release:
		return provisioner.RealizeResult{StorePath: "/nix/store/x"}, nil
	}
}
func (b *blockingProvisioner) Prune(context.Context, []provisioner.EnvRef) error { return nil }

func pendingEnv(name string) *floxv1alpha1.FloxEnv {
	return &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
		Spec: floxv1alpha1.FloxEnvSpec{
			Manifest: &runtime.RawExtension{Raw: []byte(`{"install":{"x":{"pkg-path":"x"}}}`)},
		},
		Status: floxv1alpha1.FloxEnvStatus{Conditions: []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pending", Message: "queued",
			LastTransitionTime: metav1.Now(), ObservedGeneration: 1,
		}}},
	}
}

// A realise that never returns used to hold its reconcile, and the env at Realizing, for good:
// measured on bioskop-mgmt, four envs from 22:48:16Z for nine hours. Bounded, it fails into
// RealizeFailed and the returned error's backoff.
func TestReconcile_AHungRealizeTimesOut(t *testing.T) {
	env := pendingEnv("carrier")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}).Build()
	bp := &blockingProvisioner{release: make(chan struct{})}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: bp,
		RealizeTimeout: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := r.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: types.NamespacedName{Name: "carrier", Namespace: "ns"}})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("reconcile error = %v, want the realise's deadline", err)
		}
	case <-time.After(5 * time.Second):
		close(bp.release)
		t.Fatal("a hung realise still holds its reconcile")
	}
	var got floxv1alpha1.FloxEnv
	_ = c.Get(context.Background(), types.NamespacedName{Name: "carrier", Namespace: "ns"}, &got)
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Reason != "RealizeFailed" {
		t.Fatalf("Ready = %+v, want RealizeFailed", cond)
	}
}

// Each realise is a node-side nix build; a hundred at once is a hundred builds on one node. At most
// RealizeSlots run together; the others wait for a slot.
func TestReconcile_RealisationsShareABoundedNumberOfSlots(t *testing.T) {
	names := []string{"a", "b", "c", "d"}
	objs := make([]floxv1alpha1.FloxEnv, 0, len(names))
	builder := fake.NewClientBuilder().WithScheme(scheme(t)).WithStatusSubresource(&floxv1alpha1.FloxEnv{})
	for _, n := range names {
		objs = append(objs, *pendingEnv(n))
	}
	for i := range objs {
		builder = builder.WithObjects(&objs[i])
	}
	c := builder.Build()
	bp := &blockingProvisioner{release: make(chan struct{})}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: bp,
		RealizeTimeout: time.Minute, RealizeSlots: 2}

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_, _ = r.Reconcile(context.Background(),
				ctrl.Request{NamespacedName: types.NamespacedName{Name: n, Namespace: "ns"}})
		}(n)
	}
	time.Sleep(300 * time.Millisecond)
	if p := bp.peak.Load(); p != 2 {
		close(bp.release)
		wg.Wait()
		t.Fatalf("%d realisations ran at once, want the 2 slots", p)
	}
	close(bp.release)
	wg.Wait()
}

// A status-only update (the reconcile's own writes) does not re-enqueue: only a spec change bumps
// the generation, or a relock annotation asks for a new realise.
func TestWatch_IgnoresStatusOnlyUpdates(t *testing.T) {
	old := pendingEnv("a")
	statusOnly := old.DeepCopy()
	statusOnly.Status.Conditions[0].Message = "realising"
	if realisationChange().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusOnly}) {
		t.Error("a status-only update re-enqueues the env")
	}
	specChange := old.DeepCopy()
	specChange.Generation = 2
	if !realisationChange().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: specChange}) {
		t.Error("a spec change (new generation) is dropped")
	}
	relock := old.DeepCopy()
	relock.Annotations = map[string]string{relockAnnotation: "now"}
	if !realisationChange().Update(event.UpdateEvent{ObjectOld: old, ObjectNew: relock}) {
		t.Error("a relock annotation is dropped")
	}
}

// A realise that fails at once returns its error, so the workqueue's per-item exponential backoff
// paces the retries. What bypassed it was the watch: each pass flipped the status Realizing →
// RealizeFailed, and each flip re-enqueued the env at once (measured by 🎛️ on bioskop-mgmt: about
// 16 000 respawned `flox activate` per minute for 5 envs). TestWatch_IgnoresStatusOnlyUpdates holds
// the other half.
func TestReconcile_AFastFailingRealizeIsLeftToTheBackoff(t *testing.T) {
	env := pendingEnv("kube")
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}).Build()
	fp := &fakeProvisioner{err: errors.New("flox activate: exit status 1")}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: fp}
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "kube", Namespace: "ns"}})
	if err == nil {
		t.Fatal("a failed realise returned no error: the workqueue cannot back it off")
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatalf("result = %+v: an explicit requeue bypasses the backoff", res)
	}
}
