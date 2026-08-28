package controller

import (
	"context"
	"encoding/json"
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

// FloxEnvReconciler realises a FloxEnv onto the LOCAL node's nix store.
//
// Node-agent model: one instance per node (run as a DaemonSet); each instance
// reconciles only its own node (NodeName, from the Downward API) and patches that
// node's entry in status.realized. Host ops go through Provisioner (the exec impl
// shells to flox/nix/ctr; tests substitute a fake).
type FloxEnvReconciler struct {
	client.Client
	NodeName    string
	Provisioner provisioner.Provisioner
}

// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxenvs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxenvs/status,verbs=get;update;patch

// Reconcile realises the desired FloxEnv onto this node's host /nix/store.
func (r *FloxEnvReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var env floxv1alpha1.FloxEnv
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// spec.folder defaults to the namespace; spec.consumption to overlay.
	folder := env.Spec.Folder
	if folder == "" {
		folder = env.Namespace
	}
	consumption := env.Spec.Consumption
	if consumption == "" {
		consumption = "overlay"
	}

	manifestTOML, err := manifestToTOML(env.Spec.Manifest)
	if err != nil {
		return r.fail(ctx, &env, "SerializeFailed", err)
	}

	res, err := r.Provisioner.Realize(ctx, provisioner.RealizeRequest{
		Ref:          provisioner.EnvRef{Folder: folder, Name: env.Name},
		ManifestTOML: manifestTOML,
		Lock:         env.Status.Lock,
		Consumption:  consumption,
	})
	if err != nil {
		l.Error(err, "realize failed", "env", req.NamespacedName, "node", r.NodeName)
		return r.fail(ctx, &env, "RealizeFailed", err)
	}

	base := env.DeepCopy()
	upsertRealization(&env.Status, floxv1alpha1.NodeRealization{
		Node:               r.NodeName,
		Ready:              true,
		StorePath:          res.StorePath,
		ObservedGeneration: env.Generation,
	})
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Realized",
		Message:            fmt.Sprintf("realized on %s at %s", r.NodeName, res.StorePath),
		ObservedGeneration: env.Generation,
	})
	if err := r.Status().Patch(ctx, &env, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	l.Info("reconciled FloxEnv",
		"env", req.NamespacedName, "node", r.NodeName,
		"folder", folder, "consumption", consumption, "storePath", res.StorePath)
	return ctrl.Result{}, nil
}

// fail records a Ready=False condition (best-effort) and returns the cause so the
// work item requeues with backoff.
func (r *FloxEnvReconciler) fail(ctx context.Context, env *floxv1alpha1.FloxEnv, reason string, cause error) (ctrl.Result, error) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            cause.Error(),
		ObservedGeneration: env.Generation,
	})
	_ = r.Status().Patch(ctx, env, client.MergeFrom(base))
	return ctrl.Result{}, cause
}

// manifestToTOML transposes spec.manifest (stored as JSON by the API server) into
// manifest.toml — a faithful shape transposition, not a re-modelling.
func manifestToTOML(raw *runtime.RawExtension) ([]byte, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return []byte{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw.Raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal spec.manifest: %w", err)
	}
	out, err := toml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest.toml: %w", err)
	}
	return out, nil
}

// upsertRealization replaces this node's entry (or appends it), keeping
// status.realized a per-node map keyed by node.
func upsertRealization(st *floxv1alpha1.FloxEnvStatus, nr floxv1alpha1.NodeRealization) {
	for i := range st.Realized {
		if st.Realized[i].Node == nr.Node {
			st.Realized[i] = nr
			return
		}
	}
	st.Realized = append(st.Realized, nr)
}

// SetupWithManager wires the reconciler to watch FloxEnv resources.
func (r *FloxEnvReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&floxv1alpha1.FloxEnv{}).
		Complete(r)
}
