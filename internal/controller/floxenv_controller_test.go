package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

type fakeProvisioner struct {
	got    provisioner.RealizeRequest
	result provisioner.RealizeResult
	err    error
}

func (f *fakeProvisioner) Realize(_ context.Context, req provisioner.RealizeRequest) (provisioner.RealizeResult, error) {
	f.got = req
	return f.result, f.err
}
func (f *fakeProvisioner) Prune(context.Context, []provisioner.EnvRef) error { return nil }

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := floxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func TestReconcile_RealizesAndPatchesStatus(t *testing.T) {
	env := &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: "kdns", Namespace: "networking", Generation: 3},
		Spec: floxv1alpha1.FloxEnvSpec{
			// no folder set → must default to the namespace
			Manifest: &runtime.RawExtension{Raw: []byte(`{"install":{"kdns":{"flake":"x"}}}`)},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}).
		Build()

	fp := &fakeProvisioner{result: provisioner.RealizeResult{StorePath: "/nix/store/abc-env"}}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: fp}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "kdns", Namespace: "networking"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// folder defaulted to the namespace, and the manifest was transposed to TOML.
	if fp.got.Ref.Folder != "networking" {
		t.Errorf("folder = %q, want networking", fp.got.Ref.Folder)
	}
	if !strings.Contains(string(fp.got.ManifestTOML), "kdns") {
		t.Errorf("manifest TOML missing package: %q", fp.got.ManifestTOML)
	}
	if fp.got.Consumption != "overlay" {
		t.Errorf("consumption = %q, want overlay (default)", fp.got.Consumption)
	}

	// status.realized carries this node's entry; Ready is True.
	var got floxv1alpha1.FloxEnv
	if err := c.Get(context.Background(), types.NamespacedName{Name: "kdns", Namespace: "networking"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Status.Realized) != 1 || got.Status.Realized[0].Node != "node-a" ||
		got.Status.Realized[0].StorePath != "/nix/store/abc-env" || !got.Status.Realized[0].Ready {
		t.Errorf("realized = %+v", got.Status.Realized)
	}
	if !meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Errorf("Ready condition not true: %+v", got.Status.Conditions)
	}
}

func TestReconcile_RealizeFailureSetsNotReady(t *testing.T) {
	env := &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: "kdns", Namespace: "networking"},
		Spec:       floxv1alpha1.FloxEnvSpec{Consumption: "image", Folder: "flox-system"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme(t)).
		WithObjects(env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}).
		Build()

	fp := &fakeProvisioner{err: context.DeadlineExceeded}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: fp}

	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "kdns", Namespace: "networking"}}); err == nil {
		t.Fatal("expected error to requeue")
	}
	if fp.got.Consumption != "image" || fp.got.Ref.Folder != "flox-system" {
		t.Errorf("req = %+v", fp.got)
	}

	var got floxv1alpha1.FloxEnv
	if err := c.Get(context.Background(), types.NamespacedName{Name: "kdns", Namespace: "networking"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, "Ready") {
		t.Errorf("Ready should be false on failure: %+v", got.Status.Conditions)
	}
}

func TestReconcile_ResolvesFloxCatalogRefAndCapturesLock(t *testing.T) {
	flake := &floxv1alpha1.FloxCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue", Namespace: "networking"},
		Status: floxv1alpha1.FloxCatalogStatus{
			FlakeRef: "tarball+http://sc.flux/gitrepository/flux-system/rke2lab/deadbeef.tar.gz?dir=runtime/flox",
		},
	}
	env := &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: "kdns", Namespace: "networking"},
		Spec: floxv1alpha1.FloxEnvSpec{
			Manifest: &runtime.RawExtension{Raw: []byte(`{"install":{"kdns":{"flake":"floxcatalog:catalogue#kdns"}}}`)},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(flake, env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}, &floxv1alpha1.FloxCatalog{}).Build()

	fp := &fakeProvisioner{result: provisioner.RealizeResult{StorePath: "/nix/store/x", Lock: "locked!"}}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: fp}
	if _, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "kdns", Namespace: "networking"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// the floxcatalog: pseudo-scheme was rewritten to the concrete tarball ref + output.
	toml := string(fp.got.ManifestTOML)
	if !strings.Contains(toml, "deadbeef.tar.gz?dir=runtime/flox#kdns") {
		t.Errorf("flake ref not resolved in TOML: %q", toml)
	}
	if strings.Contains(toml, "floxcatalog:") {
		t.Errorf("pseudo-scheme leaked to flox: %q", toml)
	}

	// the produced lock was captured as the CR pin-of-record.
	var got floxv1alpha1.FloxEnv
	if err := c.Get(context.Background(), types.NamespacedName{Name: "kdns", Namespace: "networking"}, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Lock != "locked!" {
		t.Errorf("status.lock = %q, want locked!", got.Status.Lock)
	}
}

func TestReconcile_WaitsForUnresolvedFlake(t *testing.T) {
	flake := &floxv1alpha1.FloxCatalog{ // no status.flakeRef → artifact not resolved yet
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue", Namespace: "networking"},
	}
	env := &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: "kdns", Namespace: "networking"},
		Spec: floxv1alpha1.FloxEnvSpec{
			Manifest: &runtime.RawExtension{Raw: []byte(`{"install":{"kdns":{"flake":"floxcatalog:catalogue#kdns"}}}`)},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(flake, env).
		WithStatusSubresource(&floxv1alpha1.FloxEnv{}).Build()

	fp := &fakeProvisioner{}
	r := &FloxEnvReconciler{Client: c, NodeName: "node-a", Provisioner: fp}
	res, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Name: "kdns", Namespace: "networking"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("expected requeue while the FloxCatalog is unresolved")
	}
	if fp.got.ManifestTOML != nil {
		t.Errorf("provisioner must not run while waiting: %+v", fp.got)
	}
}
