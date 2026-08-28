package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

// floxSchemaVersion is the manifest.toml schema-version the controller stamps onto a serialised
// spec.manifest when the maintainer didn't set it (they shouldn't — it's flox plumbing).
const floxSchemaVersion = "1.14.0"

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
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxflakes,verbs=get;list;watch

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

	manifest, err := parseManifest(env.Spec.Manifest)
	if err != nil {
		return r.fail(ctx, &env, "SerializeFailed", err)
	}
	if waiting, err := r.resolveFlakeInstalls(ctx, env.Namespace, manifest); err != nil {
		return r.fail(ctx, &env, "FlakeResolveFailed", err)
	} else if waiting {
		return r.waitForFlake(ctx, &env)
	}
	manifestTOML, err := manifestToTOML(manifest)
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
	if res.Lock != "" {
		env.Status.Lock = res.Lock // pin-of-record; fed back verbatim (RealizeRequest.Lock) next reconcile
	}
	upsertRealization(&env.Status, floxv1alpha1.NodeRealization{
		Node:               r.NodeName,
		Ready:              true,
		StorePath:          res.StorePath,
		EnvPath:            res.EnvPath,
		GcrootPath:         res.GcrootPath,
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

// waitForFlake records that a referenced FloxFlake has not resolved its artifact yet and
// requeues — no error, so no exponential-backoff spam for an expected transient wait.
func (r *FloxEnvReconciler) waitForFlake(ctx context.Context, env *floxv1alpha1.FloxEnv) (ctrl.Result, error) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "WaitingForFlake",
		Message:            "referenced FloxFlake has not resolved an artifact yet",
		ObservedGeneration: env.Generation,
	})
	_ = r.Status().Patch(ctx, env, client.MergeFrom(base))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// parseManifest decodes spec.manifest (stored as JSON by the API server) into a mutable map
// so the controller can resolve floxflake: install refs before serialising to TOML.
func parseManifest(raw *runtime.RawExtension) (map[string]any, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw.Raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal spec.manifest: %w", err)
	}
	return m, nil
}

// manifestToTOML transposes the (resolved) manifest map into manifest.toml — a faithful
// shape transposition, not a re-modelling.
func manifestToTOML(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte{}, nil
	}
	// schema-version is flox plumbing, not manifest content the maintainer authors — inject it
	// so spec.manifest stays clean. TODO(on-node): track the node's flox rather than pinning.
	if _, ok := m["schema-version"]; !ok {
		m["schema-version"] = floxSchemaVersion
	}
	out, err := toml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest.toml: %w", err)
	}
	return out, nil
}

const floxFlakeScheme = "floxflake:"

// floxFlakeRef is a parsed "floxflake:[<namespace>/]<name>#<output>" install reference — a
// controller PSEUDO-scheme, not a nix scheme. It names a FloxFlake whose resolved
// status.flakeRef the controller substitutes in before flox ever sees the manifest.
type floxFlakeRef struct {
	namespace string
	name      string
	output    string
}

func parseFloxFlakeRef(s string) (floxFlakeRef, error) {
	body := strings.TrimPrefix(s, floxFlakeScheme)
	var output string
	if i := strings.Index(body, "#"); i >= 0 {
		output, body = body[i+1:], body[:i]
	}
	var ns, name string
	if i := strings.Index(body, "/"); i >= 0 {
		ns, name = body[:i], body[i+1:]
	} else {
		name = body
	}
	if name == "" {
		return floxFlakeRef{}, fmt.Errorf("invalid ref %q (want floxflake:[<ns>/]<name>#<output>)", s)
	}
	return floxFlakeRef{namespace: ns, name: name, output: output}, nil
}

// resolveFlakeInstalls rewrites any install.<id>.flake using the "floxflake:" pseudo-scheme
// to the concrete nix ref from the referenced FloxFlake's status.flakeRef (+ the requested
// output). Returns waiting=true if a referenced FloxFlake exists but has not resolved an
// artifact yet, so the caller requeues instead of realising a half-resolved manifest.
func (r *FloxEnvReconciler) resolveFlakeInstalls(ctx context.Context, envNS string, m map[string]any) (bool, error) {
	install, ok := m["install"].(map[string]any)
	if !ok {
		return false, nil
	}
	for id, v := range install {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := entry["flake"].(string)
		if !ok || !strings.HasPrefix(raw, floxFlakeScheme) {
			continue
		}
		ref, err := parseFloxFlakeRef(raw)
		if err != nil {
			return false, fmt.Errorf("install.%s.flake: %w", id, err)
		}
		ns := ref.namespace
		if ns == "" {
			ns = envNS
		}
		var flake floxv1alpha1.FloxFlake
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.name}, &flake); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Errorf("install.%s: FloxFlake %s/%s not found", id, ns, ref.name)
			}
			return false, err
		}
		if flake.Status.FlakeRef == "" {
			return true, nil // referenced FloxFlake has not resolved an artifact yet
		}
		concrete := flake.Status.FlakeRef
		if ref.output != "" {
			concrete += "#" + ref.output
		}
		entry["flake"] = concrete
	}
	return false, nil
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
