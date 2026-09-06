package controller

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/floxenv"
)

func podgateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := floxv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("flox scheme: %v", err)
	}
	return s
}

func gatedPod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "app", Annotations: annotations},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: floxenv.SchedulingGateName}},
			Containers:      []corev1.Container{{Name: "app"}},
		},
	}
}

func floxEnvOn(name, folder string, generation int64, realized ...floxv1alpha1.NodeRealization) *floxv1alpha1.FloxEnv {
	return &floxv1alpha1.FloxEnv{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "flox-system", Generation: generation},
		Spec:       floxv1alpha1.FloxEnvSpec{Folder: folder},
		Status:     floxv1alpha1.FloxEnvStatus{Realized: realized},
	}
}

func reconcilePod(t *testing.T, r *PodGateReconciler) *corev1.Pod {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "app", Name: "consumer"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var out corev1.Pod
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "consumer"}, &out); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	return &out
}

func hostnameValues(pod *corev1.Pod) []string {
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return nil
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return nil
	}
	var vals []string
	for _, term := range na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, e := range term.MatchExpressions {
			if e.Key == hostnameLabel {
				vals = append(vals, e.Values...)
			}
		}
	}
	sort.Strings(vals)
	return vals
}

func TestPodGate_StaysGatedUntilRealized(t *testing.T) {
	pod := gatedPod(map[string]string{floxenv.AnnotationPrefix + "app": "networking/kdns"})
	// env exists but not realised on any node yet
	env := floxEnvOn("kdns", "networking", 1)
	c := fake.NewClientBuilder().WithScheme(podgateScheme(t)).WithObjects(pod, env).Build()
	r := &PodGateReconciler{Client: c}

	out := reconcilePod(t, r)
	if !hasGate(out) {
		t.Fatal("pod should stay gated while env is unrealised")
	}
	if out.Spec.Affinity != nil {
		t.Errorf("no affinity should be injected while gated: %+v", out.Spec.Affinity)
	}
}

func TestPodGate_StaleGenerationStaysGated(t *testing.T) {
	pod := gatedPod(map[string]string{floxenv.AnnotationPrefix + "app": "networking/kdns"})
	// realised, but at a PRIOR generation (spec bumped to 2, node still on 1)
	env := floxEnvOn("kdns", "networking", 2,
		floxv1alpha1.NodeRealization{Node: "node-1", Ready: true, ObservedGeneration: 1})
	c := fake.NewClientBuilder().WithScheme(podgateScheme(t)).WithObjects(pod, env).Build()
	r := &PodGateReconciler{Client: c}

	if out := reconcilePod(t, r); !hasGate(out) {
		t.Fatal("pod must stay gated when realisation lags the current generation")
	}
}

func TestPodGate_ReleasesAndPinsToReadyNode(t *testing.T) {
	pod := gatedPod(map[string]string{floxenv.AnnotationPrefix + "app": "networking/kdns"})
	env := floxEnvOn("kdns", "networking", 3,
		floxv1alpha1.NodeRealization{Node: "node-1", Ready: true, ObservedGeneration: 3})
	c := fake.NewClientBuilder().WithScheme(podgateScheme(t)).WithObjects(pod, env).Build()
	r := &PodGateReconciler{Client: c}

	out := reconcilePod(t, r)
	if hasGate(out) {
		t.Fatal("gate must be removed once realised at current generation")
	}
	if got := hostnameValues(out); len(got) != 1 || got[0] != "node-1" {
		t.Errorf("affinity must pin to the ready node, got %v", got)
	}
}

func TestPodGate_MultiEnvIntersection(t *testing.T) {
	// The pod needs BOTH envs; only node-2 has both realised at the current generation.
	pod := gatedPod(map[string]string{
		floxenv.AnnotationPrefix + "app":  "networking/kdns",
		floxenv.AnnotationPrefix + "mesh": "mesh/relay",
	})
	kdns := floxEnvOn("kdns", "networking", 1,
		floxv1alpha1.NodeRealization{Node: "node-1", Ready: true, ObservedGeneration: 1},
		floxv1alpha1.NodeRealization{Node: "node-2", Ready: true, ObservedGeneration: 1})
	relay := floxEnvOn("relay", "mesh", 1,
		floxv1alpha1.NodeRealization{Node: "node-2", Ready: true, ObservedGeneration: 1})
	c := fake.NewClientBuilder().WithScheme(podgateScheme(t)).WithObjects(pod, kdns, relay).Build()
	r := &PodGateReconciler{Client: c}

	out := reconcilePod(t, r)
	if hasGate(out) {
		t.Fatal("gate must be removed once both envs share a ready node")
	}
	if got := hostnameValues(out); len(got) != 1 || got[0] != "node-2" {
		t.Errorf("affinity must pin to the intersection node, got %v", got)
	}
}

func TestPodGate_MultiEnvNoCommonNodeStaysGated(t *testing.T) {
	pod := gatedPod(map[string]string{
		floxenv.AnnotationPrefix + "app":  "networking/kdns",
		floxenv.AnnotationPrefix + "mesh": "mesh/relay",
	})
	kdns := floxEnvOn("kdns", "networking", 1,
		floxv1alpha1.NodeRealization{Node: "node-1", Ready: true, ObservedGeneration: 1})
	relay := floxEnvOn("relay", "mesh", 1,
		floxv1alpha1.NodeRealization{Node: "node-2", Ready: true, ObservedGeneration: 1})
	c := fake.NewClientBuilder().WithScheme(podgateScheme(t)).WithObjects(pod, kdns, relay).Build()
	r := &PodGateReconciler{Client: c}

	if out := reconcilePod(t, r); !hasGate(out) {
		t.Fatal("pod must stay gated when no single node has both envs")
	}
}
