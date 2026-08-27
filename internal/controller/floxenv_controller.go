package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
)

// FloxEnvReconciler reconciles a FloxEnv onto the LOCAL node's nix store.
//
// Node-agent model: one instance per node (run as a DaemonSet); each instance
// reconciles only its own node (NodeName, from the Downward API) and patches that
// node's entry in status.realized.
//
// SKELETON: the realise / GC-root / include-layout logic is TODO — see the rke2lab
// spec "Flox Store-Resolved Runtime" §Runtime env delivery.
type FloxEnvReconciler struct {
	client.Client
	// NodeName is the node this agent runs on.
	NodeName string
}

// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxenvs,verbs=get;list;watch
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxenvs/status,verbs=get;update;patch

// Reconcile realises the desired FloxEnv onto this node's host /nix/store.
func (r *FloxEnvReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var env floxv1alpha1.FloxEnv
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// TODO(scaffold): via the node's nix daemon, realise the closures pinned by
	// env.Spec.Lock onto the host /nix/store (substitute from the builder), write the
	// flox-runtime GC-roots at <envroot>/<folder>/<name>, render the [include] layout
	// from env.Spec.Includes, then patch status.realized for r.NodeName.
	l.Info("reconcile FloxEnv (skeleton)",
		"env", req.NamespacedName, "node", r.NodeName, "folder", env.Spec.Folder)

	return ctrl.Result{}, nil
}

// SetupWithManager wires the reconciler to watch FloxEnv resources.
func (r *FloxEnvReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&floxv1alpha1.FloxEnv{}).
		Complete(r)
}
