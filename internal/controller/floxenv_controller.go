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
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

// floxSchemaVersion is the manifest.toml schema-version the controller stamps onto a serialised
// spec.manifest when the maintainer didn't set it (they shouldn't — it's flox plumbing).
const floxSchemaVersion = "1.14.0"

// maxConcurrentReconciles lets independent FloxEnvs realise in parallel. It is deliberately high,
// NOT a tuned job count: the controller does not manage parallelism — the node's nix throttles the
// actual concurrent BUILDS via its own max-jobs/cores. spec.dependsOn still serialises where a real
// order exists (WaitingForDeps).
const maxConcurrentReconciles = 100

// relockAnnotation forces a fresh re-lock: when its value differs from status.RelockToken, the
// reconciler drops the pinned lock so Realize re-locks from scratch (see FloxEnvStatus.RelockToken).
// Patch it (e.g. to a timestamp) to re-pull a FloxEnv's flake inputs without editing spec.
const relockAnnotation = "flox.seedmatic.io/relock"

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
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxcatalogs,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets;statefulsets,verbs=get;list;watch;patch

// Reconcile realises the desired FloxEnv onto this node's host /nix/store.
func (r *FloxEnvReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var env floxv1alpha1.FloxEnv
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// First touch: publish Pending and requeue. Realise is serial + blocking (each is a node nix
	// build), so only one env is Realizing at a time and the rest sit in the workqueue — without
	// this they would show a blank phase until their turn. The requeue then does the real work.
	if meta.FindStatusCondition(env.Status.Conditions, "Ready") == nil {
		return r.markPending(ctx, &env)
	}

	// Idempotent short-circuit: this node already realised the current generation and no relock is
	// pending → nothing to do. Returning WITHOUT a status write is what keeps the reconcile from
	// looping — a Realizing/Realized flip on every pass would re-enqueue us (the For-watch reacts to
	// status updates) and re-run the carrier's slow containerize each time.
	if r.alreadyRealized(&env) {
		return ctrl.Result{}, nil
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

	// Hold until every declared dependency is Realized, so parallel realisation never races an env's
	// flox `include` of another's materialised tree. Requeue (not block) — the worker stays free for
	// the envs that CAN realise now.
	if unmet := r.unmetDeps(ctx, &env); len(unmet) > 0 {
		return r.waitForDeps(ctx, &env, unmet)
	}

	// A change to the relock annotation drops the pinned lock so this realise re-locks from scratch
	// (pulls fresh flake inputs) instead of re-using status.Lock. Recorded into status after a
	// successful realise, so the force fires exactly once per distinct annotation value.
	relock := env.Annotations[relockAnnotation]
	lock := env.Status.Lock
	previousLock := env.Status.Lock // the last realized pin, to detect a real change post-realise
	if relock != env.Status.RelockToken {
		lock = ""
	}

	// Surface that a (possibly long) node-side nix build is in flight: the reconcile BLOCKS in
	// Realize, so without this the env would keep showing its prior phase until the build returns.
	// Best-effort — a failed patch only loses the transient Realizing hint.
	r.markRealizing(ctx, &env)

	res, err := r.Provisioner.Realize(ctx, provisioner.RealizeRequest{
		Ref:          provisioner.EnvRef{Folder: folder, Name: env.Name},
		ManifestTOML: manifestTOML,
		Lock:         lock,
		Consumption:  consumption,
	})
	if err != nil {
		l.Error(err, "realize failed", "env", req.NamespacedName, "node", r.NodeName)
		return r.fail(ctx, &env, "RealizeFailed", err)
	}

	// Close the delivery loop: a re-lock only makes the new binary AVAILABLE — a running consumer
	// keeps the old closure until it restarts (the NRI plugin resolves the env at container start).
	// So when the realized lock actually CHANGED, roll the workloads that consume this env. Done
	// BEFORE the status patch (below) so a failure leaves status.Lock at the old pin and the next
	// reconcile re-realises (nix cache-hit) and retries the roll.
	if res.Lock != "" && res.Lock != previousLock {
		if err := r.restartConsumers(ctx, &env, res.Lock); err != nil {
			return r.fail(ctx, &env, "RestartConsumersFailed", err)
		}
	}

	base := env.DeepCopy()
	if res.Lock != "" {
		env.Status.Lock = res.Lock // pin-of-record; fed back verbatim (RealizeRequest.Lock) next reconcile
	}
	env.Status.RelockToken = relock // honored: this force (if any) fired, don't repeat it
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

// waitForFlake records that a referenced FloxCatalog has not resolved its artifact yet and
// requeues — no error, so no exponential-backoff spam for an expected transient wait.
func (r *FloxEnvReconciler) waitForFlake(ctx context.Context, env *floxv1alpha1.FloxEnv) (ctrl.Result, error) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "WaitingForFlake",
		Message:            "referenced FloxCatalog has not resolved an artifact yet",
		ObservedGeneration: env.Generation,
	})
	_ = r.Status().Patch(ctx, env, client.MergeFrom(base))
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// markPending publishes a Ready=False/Pending condition on an env's first touch and requeues, so a
// queued env (waiting behind the serial, blocking realise) shows "Pending" instead of a blank phase.
func (r *FloxEnvReconciler) markPending(
	ctx context.Context, env *floxv1alpha1.FloxEnv) (ctrl.Result, error) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Pending",
		Message:            "queued for realisation",
		ObservedGeneration: env.Generation,
	})
	if err := r.Status().Patch(ctx, env, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// markRealizing publishes a Ready=False/Realizing condition before the (blocking) Realize, so the
// env's Phase column reads "Realizing" while the node-side build runs. Best-effort: a failed patch
// only loses the transient hint, and the terminal condition (Realized / *Failed) overwrites it.
func (r *FloxEnvReconciler) markRealizing(ctx context.Context, env *floxv1alpha1.FloxEnv) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Realizing",
		Message:            "realising the env closure on " + r.NodeName,
		ObservedGeneration: env.Generation,
	})
	_ = r.Status().Patch(ctx, env, client.MergeFrom(base))
}

// alreadyRealized reports whether this node has realised the env's CURRENT generation with no relock
// pending — the steady state where a reconcile has nothing to do. Keeping it side-effect-free (no
// status write) is what stops the reconcile from re-enqueuing itself.
func (r *FloxEnvReconciler) alreadyRealized(env *floxv1alpha1.FloxEnv) bool {
	if env.Annotations[relockAnnotation] != env.Status.RelockToken {
		return false // a relock is pending — must re-realise
	}
	for _, nr := range env.Status.Realized {
		if nr.Node == r.NodeName && nr.Ready && nr.ObservedGeneration == env.Generation {
			return true
		}
	}
	return false
}

// unmetDeps returns the spec.dependsOn FloxEnvs (same namespace) that are not yet Ready — a missing
// dependency counts as unmet.
func (r *FloxEnvReconciler) unmetDeps(ctx context.Context, env *floxv1alpha1.FloxEnv) []string {
	var unmet []string
	for _, name := range env.Spec.DependsOn {
		var dep floxv1alpha1.FloxEnv
		if err := r.Get(
			ctx, client.ObjectKey{Namespace: env.Namespace, Name: name}, &dep); err != nil {
			unmet = append(unmet, name)
			continue
		}
		if !meta.IsStatusConditionTrue(dep.Status.Conditions, "Ready") {
			unmet = append(unmet, name)
		}
	}
	return unmet
}

// waitForDeps records WaitingForDeps and requeues (no error, no backoff spam) until the declared
// dependencies are Ready.
func (r *FloxEnvReconciler) waitForDeps(
	ctx context.Context, env *floxv1alpha1.FloxEnv, unmet []string) (ctrl.Result, error) {
	base := env.DeepCopy()
	meta.SetStatusCondition(&env.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "WaitingForDeps",
		Message:            "waiting for FloxEnvs to be Ready: " + strings.Join(unmet, ", "),
		ObservedGeneration: env.Generation,
	})
	_ = r.Status().Patch(ctx, env, client.MergeFrom(base))
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// parseManifest decodes spec.manifest (stored as JSON by the API server) into a mutable map
// so the controller can resolve floxcatalog: install refs before serialising to TOML.
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

const floxCatalogScheme = "floxcatalog:"

// floxCatalogRef is a parsed "floxcatalog:[<namespace>/]<name>#<output>" install reference — a
// controller PSEUDO-scheme, not a nix scheme. It names a FloxCatalog whose resolved
// status.flakeRef the controller substitutes in before flox ever sees the manifest.
type floxCatalogRef struct {
	namespace string
	name      string
	output    string
}

func parseFloxCatalogRef(s string) (floxCatalogRef, error) {
	body := strings.TrimPrefix(s, floxCatalogScheme)
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
		return floxCatalogRef{}, fmt.Errorf("invalid ref %q (want floxcatalog:[<ns>/]<name>#<output>)", s)
	}
	return floxCatalogRef{namespace: ns, name: name, output: output}, nil
}

// resolveFlakeInstalls rewrites any install.<id>.flake using the "floxcatalog:" pseudo-scheme
// to the concrete nix ref from the referenced FloxCatalog's status.flakeRef (+ the requested
// output). Returns waiting=true if a referenced FloxCatalog exists but has not resolved an
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
		if !ok || !strings.HasPrefix(raw, floxCatalogScheme) {
			continue
		}
		ref, err := parseFloxCatalogRef(raw)
		if err != nil {
			return false, fmt.Errorf("install.%s.flake: %w", id, err)
		}
		ns := ref.namespace
		if ns == "" {
			ns = envNS
		}
		var flake floxv1alpha1.FloxCatalog
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.name}, &flake); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Errorf("install.%s: FloxCatalog %s/%s not found", id, ns, ref.name)
			}
			return false, err
		}
		if flake.Status.FlakeRef == "" {
			return true, nil // referenced FloxCatalog has not resolved an artifact yet
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
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}
