package controller

import (
	"context"
	"fmt"
	"net"
	neturl "net/url"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
)

// gitRepositoryGVK is the Flux source-controller GitRepository. Read as UNSTRUCTURED so
// the controller carries no dependency on the Flux Go module — it only needs
// status.artifact.{url,revision}.
var gitRepositoryGVK = schema.GroupVersionKind{
	Group:   "source.toolkit.fluxcd.io",
	Version: "v1",
	Kind:    "GitRepository",
}

// FloxFlakeReconciler resolves a FloxFlake to a concrete nix flake reference derived from
// the referenced Flux source's reconciled artifact — an in-cluster tarball at the EXACT
// reconciled commit (no external fetch, no token: Flux already fetched it). This is
// cluster-scoped, node-independent work; on multi-node it may run redundantly on each
// node-agent (idempotent) until split into a leader-elected cluster manager.
type FloxFlakeReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxflakes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxflakes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=get;list;watch

// Reconcile derives status.flakeRef from the referenced GitRepository's artifact.
func (r *FloxFlakeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var flake floxv1alpha1.FloxFlake
	if err := r.Get(ctx, req.NamespacedName, &flake); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	srcNS := flake.Spec.SourceRef.Namespace
	if srcNS == "" {
		srcNS = flake.Namespace
	}

	var src unstructured.Unstructured
	src.SetGroupVersionKind(gitRepositoryGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: srcNS, Name: flake.Spec.SourceRef.Name}, &src); err != nil {
		return r.notReady(ctx, &flake, "SourceNotFound", err)
	}

	url, _, _ := unstructured.NestedString(src.Object, "status", "artifact", "url")
	if url == "" {
		return r.notReady(ctx, &flake, "ArtifactNotReady",
			fmt.Errorf("GitRepository %s/%s has no status.artifact.url yet", srcNS, flake.Spec.SourceRef.Name))
	}
	revision, _, _ := unstructured.NestedString(src.Object, "status", "artifact", "revision")

	// The Flux artifact is an in-cluster tarball at the reconciled commit → a nix tarball
	// flake ref; dir scopes the flake root within the source tree. nix fetches it ON THE NODE
	// (via nsenter), whose netns routes ClusterIPs (Cilium) but has NO cluster DNS — so rewrite
	// the artifact host to its ClusterIP, resolved here IN-POD where cluster DNS works.
	reachableURL, err := nodeReachableURL(url)
	if err != nil {
		return r.notReady(ctx, &flake, "ArtifactURLUnresolvable", err)
	}
	flakeRef := "tarball+" + reachableURL
	if flake.Spec.Dir != "" {
		flakeRef += "?dir=" + flake.Spec.Dir
	}

	base := flake.DeepCopy()
	flake.Status.Revision = revision
	flake.Status.ArtifactURL = url
	flake.Status.FlakeRef = flakeRef
	meta.SetStatusCondition(&flake.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		Message:            fmt.Sprintf("resolved from GitRepository %s at %s", flake.Spec.SourceRef.Name, revision),
		ObservedGeneration: flake.Generation,
	})
	if err := r.Status().Patch(ctx, &flake, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("resolved FloxFlake", "flake", req.NamespacedName, "revision", revision, "flakeRef", flakeRef)
	return ctrl.Result{}, nil
}

func (r *FloxFlakeReconciler) notReady(ctx context.Context, flake *floxv1alpha1.FloxFlake, reason string, cause error) (ctrl.Result, error) {
	base := flake.DeepCopy()
	meta.SetStatusCondition(&flake.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: flake.Generation,
	})
	_ = r.Status().Patch(ctx, flake, client.MergeFrom(base))
	return ctrl.Result{}, cause
}

// nodeReachableURL rewrites an in-cluster artifact URL (Flux serves the tarball at
// source-controller.<ns>.svc.cluster.local) to use the resolved ClusterIP in place of the DNS
// name. The controller resolves it here IN-POD (cluster DNS works); the flake is then fetched by
// nix ON THE NODE via nsenter, whose netns routes ClusterIPs (Cilium) but carries no cluster DNS.
// source-controller serves artifacts by path, not vhost, so the IP Host header is fine. A URL
// already using an IP is returned unchanged.
func nodeReachableURL(raw string) (string, error) {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return raw, nil
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("resolve artifact host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("resolve artifact host %q: no addresses", host)
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(ips[0], port)
	} else {
		u.Host = ips[0]
	}
	return u.String(), nil
}

// flakesForSource enqueues every FloxFlake whose sourceRef names the changed GitRepository,
// so a new reconciled artifact (new commit) re-derives the flake ref.
func (r *FloxFlakeReconciler) flakesForSource(ctx context.Context, obj client.Object) []reconcile.Request {
	var list floxv1alpha1.FloxFlakeList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		f := &list.Items[i]
		ns := f.Spec.SourceRef.Namespace
		if ns == "" {
			ns = f.Namespace
		}
		if f.Spec.SourceRef.Name == obj.GetName() && ns == obj.GetNamespace() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: f.Namespace, Name: f.Name},
			})
		}
	}
	return reqs
}

// SetupWithManager watches FloxFlake + the referenced GitRepository (unstructured), so a
// new reconciled artifact re-resolves the flake ref.
func (r *FloxFlakeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	src := &unstructured.Unstructured{}
	src.SetGroupVersionKind(gitRepositoryGVK)
	return ctrl.NewControllerManagedBy(mgr).
		For(&floxv1alpha1.FloxFlake{}).
		Watches(src, handler.EnqueueRequestsFromMapFunc(r.flakesForSource)).
		Complete(r)
}
