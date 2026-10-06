package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/floxenv"
)

// hostnameLabel is the well-known node label the injected affinity pins on.
const hostnameLabel = "kubernetes.io/hostname"

// PodGateReconciler removes the flox env-ready scheduling gate from a pod once every FloxEnv it
// references is realised at the pod's current generation — but only after narrowing the pod's
// nodeAffinity to the exact node set where all those envs are realised, so the scheduler can only
// place it on a node that can actually mount them. The pod needs no node access and no API access:
// this controller owns both the realisation (the FloxEnv reconciler) and the ungate.
//
// It runs in the cluster-manager (alongside the webhook), NOT the per-node DaemonSet: gating is a
// cluster-wide decision over status.realized across nodes, not a node-local one.
type PodGateReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=flox.seedmatic.io,resources=floxenvs,verbs=get;list;watch

// Reconcile narrows a gated pod to its ready nodes and removes the gate once ready.
func (r *PodGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !hasGate(&pod) {
		return ctrl.Result{}, nil // already ungated (or never gated) — nothing to do
	}

	refs := floxenv.RefsFromAnnotations(pod.Annotations)
	if len(refs) == 0 {
		// Gated but references no env — a mismatch that should never happen; ungate rather than
		// wedge the pod forever.
		return r.release(ctx, &pod, nil)
	}

	// A node clears the gate only if EVERY referenced env is realised there at the current
	// generation: start from the first env's ready set and intersect the rest into it.
	var eligible map[string]struct{}
	for idx, ref := range refs {
		env, found, err := r.resolveFloxEnv(ctx, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !found {
			l.Info("pod gated: FloxEnv not found yet", "pod", req.NamespacedName, "folder", ref.Folder, "name", ref.Name)
			return ctrl.Result{}, nil // env not created yet; the FloxEnv watch re-enqueues on creation
		}
		nodes := readyNodesForEnv(env)
		if idx == 0 {
			eligible = nodes
		} else {
			eligible = intersect(eligible, nodes)
		}
		if len(eligible) == 0 {
			l.Info("pod gated: no node has all envs realised at current generation",
				"pod", req.NamespacedName, "waitingOn", ref.Folder+"/"+ref.Name)
			return ctrl.Result{}, nil // stay gated; a status.realized change re-enqueues
		}
	}

	l.Info("releasing gated pod onto its ready nodes", "pod", req.NamespacedName, "nodes", keys(eligible))
	return r.release(ctx, &pod, eligible)
}

// release narrows the pod's required nodeAffinity to the eligible node set (when non-empty) and
// removes the env-ready gate, in a single update. A nil/empty set removes the gate without
// narrowing (the no-env mismatch escape hatch).
func (r *PodGateReconciler) release(ctx context.Context, pod *corev1.Pod, eligibleNodes map[string]struct{}) (ctrl.Result, error) {
	if len(eligibleNodes) > 0 {
		narrowToNodes(pod, keys(eligibleNodes))
	}
	gates := pod.Spec.SchedulingGates[:0]
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != floxenv.SchedulingGateName {
			gates = append(gates, g)
		}
	}
	pod.Spec.SchedulingGates = gates
	if err := r.Update(ctx, pod); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// resolveFloxEnv finds the FloxEnv matching an annotation coordinate (spec.folder-or-namespace,
// metadata.name), cluster-wide — folder is dissociated from the k8s namespace, so the env may
// live in any namespace.
func (r *PodGateReconciler) resolveFloxEnv(ctx context.Context, ref floxenv.EnvRef) (*floxv1alpha1.FloxEnv, bool, error) {
	var list floxv1alpha1.FloxEnvList
	if err := r.List(ctx, &list); err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		if effectiveFolder(&list.Items[i]) == ref.Folder && list.Items[i].Name == ref.Name {
			return &list.Items[i], true, nil
		}
	}
	return nil, false, nil
}

// effectiveFolder is the env's host-layout folder: spec.folder, defaulting to the namespace
// (mirrors the FloxEnv reconciler).
func effectiveFolder(env *floxv1alpha1.FloxEnv) string {
	if env.Spec.Folder != "" {
		return env.Spec.Folder
	}
	return env.Namespace
}

// readyNodesForEnv is the set of nodes where the env is realised at its CURRENT generation. The
// generation check is the invariant: gcrootPath is deterministic per ref (not per generation), so
// a stale ready=true from a prior generation would otherwise pass a node whose GC-root is being
// overwritten by a re-realise.
func readyNodesForEnv(env *floxv1alpha1.FloxEnv) map[string]struct{} {
	nodes := map[string]struct{}{}
	for _, nr := range env.Status.Realized {
		if nr.Ready && nr.ObservedGeneration == env.Generation {
			nodes[nr.Node] = struct{}{}
		}
	}
	return nodes
}

// narrowToNodes ANDs "kubernetes.io/hostname In {nodes}" into the pod's required nodeAffinity.
// While a pod is gated the API server permits ADDING required node-affinity constraints
// (KEP-3521), so this narrows the schedulable set to the nodes that can mount the envs. If the
// pod has existing required terms the constraint is added to EACH (they are OR'd, so the
// constraint must hold in every branch); otherwise a single term is set.
func narrowToNodes(pod *corev1.Pod, nodes []string) {
	req := corev1.NodeSelectorRequirement{
		Key:      hostnameLabel,
		Operator: corev1.NodeSelectorOpIn,
		Values:   nodes,
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		len(na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{req}}},
		}
		return
	}
	terms := na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	for i := range terms {
		if hasRequirement(terms[i].MatchExpressions, req) {
			continue // idempotent: already narrowed
		}
		terms[i].MatchExpressions = append(terms[i].MatchExpressions, req)
	}
}

func hasRequirement(exprs []corev1.NodeSelectorRequirement, want corev1.NodeSelectorRequirement) bool {
	for _, e := range exprs {
		if e.Key == want.Key && e.Operator == want.Operator && equalStrings(e.Values, want.Values) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == floxenv.SchedulingGateName {
			return true
		}
	}
	return false
}

func intersect(a, b map[string]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		if _, ok := b[k]; ok {
			out[k] = struct{}{}
		}
	}
	return out
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// SetupWithManager watches gated pods (primary) and re-enqueues them on any FloxEnv status change
// that could satisfy their gate (so a realisation event releases the pod without polling).
func (r *PodGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	gatedOnly := predicate.NewPredicateFuncs(func(o client.Object) bool {
		pod, ok := o.(*corev1.Pod)
		return ok && hasGate(pod)
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("podgate").
		For(&corev1.Pod{}, builder.WithPredicates(gatedOnly)).
		Watches(&floxv1alpha1.FloxEnv{}, handler.EnqueueRequestsFromMapFunc(r.gatedPodsForEnv)).
		Complete(r)
}

// gatedPodsForEnv maps a changed FloxEnv to the gated pods that reference it, so a realisation
// re-enqueues exactly the waiters.
func (r *PodGateReconciler) gatedPodsForEnv(ctx context.Context, obj client.Object) []reconcile.Request {
	env, ok := obj.(*floxv1alpha1.FloxEnv)
	if !ok {
		return nil
	}
	target := floxenv.EnvRef{Folder: effectiveFolder(env), Name: env.Name}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range pods.Items {
		p := &pods.Items[i]
		if !hasGate(p) {
			continue
		}
		for _, ref := range floxenv.RefsFromAnnotations(p.Annotations) {
			if ref == target {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(p)})
				break
			}
		}
	}
	return reqs
}
