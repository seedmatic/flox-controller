package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInject_AddsWaitForAnnotatedPod(t *testing.T) {
	inj := &PodFloxWaitInjector{GcrootBase: "/nix/var/nix/gcroots/flox-runtime/env", TimeoutSeconds: 60}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"flox.dev/environment.app":   "networking/kdns",
				"flox.dev/environment.tools": "debug", // bare name → defaults to networking
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
	if err := inj.Default(context.Background(), pod); err != nil {
		t.Fatalf("default: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != floxWaitContainerName {
		t.Fatalf("init containers = %+v", pod.Spec.InitContainers)
	}
	script := pod.Spec.InitContainers[0].Command[2]
	if !strings.Contains(script, "/nix/var/nix/gcroots/flox-runtime/env/networking/kdns") {
		t.Errorf("script missing kdns gcroot: %q", script)
	}
	if !strings.Contains(script, "/nix/var/nix/gcroots/flox-runtime/env/networking/debug") {
		t.Errorf("script missing bare-name gcroot: %q", script)
	}
	if !hasVolume(pod.Spec.Volumes, floxWaitVolumeName) {
		t.Error("gcroots hostPath volume not added")
	}
}

func TestInject_Idempotent(t *testing.T) {
	inj := &PodFloxWaitInjector{GcrootBase: "/gc"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"flox.dev/environment.app": "networking/kdns"}},
	}
	_ = inj.Default(context.Background(), pod)
	_ = inj.Default(context.Background(), pod)
	if len(pod.Spec.InitContainers) != 1 {
		t.Errorf("expected exactly one flox-wait, got %d", len(pod.Spec.InitContainers))
	}
}

func TestInject_NoopWithoutAnnotations(t *testing.T) {
	inj := &PodFloxWaitInjector{GcrootBase: "/gc"}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	if err := inj.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.InitContainers) != 0 || len(pod.Spec.Volumes) != 0 {
		t.Error("should not mutate an unannotated pod")
	}
}
