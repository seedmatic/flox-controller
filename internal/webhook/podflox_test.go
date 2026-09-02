package webhook

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInject_AddsWaitForAnnotatedPod(t *testing.T) {
	inj := &PodFloxMutator{GcrootBase: "/nix/var/nix/gcroots/flox-runtime/env", TimeoutSeconds: 60}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"flox.seedmatic.io/environment.app":   "networking/kdns",
				"flox.seedmatic.io/environment.tools": "debug", // bare name → defaults to networking
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

func TestInject_InjectsFloxEnvAndToken(t *testing.T) {
	inj := &PodFloxMutator{
		GcrootBase:      "/gc",
		TokenSecretName: "floxhub-token",
		TokenSecretKey:  "token",
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"flox.seedmatic.io/environment.app": "networking/kdns"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app"},
			{Name: "sidecar"}, // not flox-annotated → left untouched
		}},
	}
	if err := inj.Default(context.Background(), pod); err != nil {
		t.Fatalf("default: %v", err)
	}
	app := pod.Spec.Containers[0]
	if envValue(app.Env, "FLOX_NONINTERACTIVE") != "1" ||
		envValue(app.Env, "_FLOX_TESTING_DISABLE_BG_SIDE_EFFECTS") != "true" {
		t.Errorf("flox knobs not injected on annotated container: %+v", app.Env)
	}
	tok := findEnv(app.Env, "FLOX_FLOXHUB_TOKEN")
	if tok == nil || tok.ValueFrom == nil || tok.ValueFrom.SecretKeyRef == nil ||
		tok.ValueFrom.SecretKeyRef.Name != "floxhub-token" || tok.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("token env not injected valueFrom the secret: %+v", tok)
	}
	if len(pod.Spec.Containers[1].Env) != 0 {
		t.Errorf("non-annotated sidecar mutated: %+v", pod.Spec.Containers[1].Env)
	}
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}

func envValue(env []corev1.EnvVar, name string) string {
	if e := findEnv(env, name); e != nil {
		return e.Value
	}
	return ""
}

func TestInject_Idempotent(t *testing.T) {
	inj := &PodFloxMutator{GcrootBase: "/gc"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"flox.seedmatic.io/environment.app": "networking/kdns"}},
	}
	_ = inj.Default(context.Background(), pod)
	_ = inj.Default(context.Background(), pod)
	if len(pod.Spec.InitContainers) != 1 {
		t.Errorf("expected exactly one flox-wait, got %d", len(pod.Spec.InitContainers))
	}
}

func TestInject_NoopWithoutAnnotations(t *testing.T) {
	inj := &PodFloxMutator{GcrootBase: "/gc"}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	if err := inj.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.InitContainers) != 0 || len(pod.Spec.Volumes) != 0 {
		t.Error("should not mutate an unannotated pod")
	}
}

func TestInject_NixBuild(t *testing.T) {
	// nil Client → ensure is skipped (the volume/mount/NIX_CONFIG are still injected).
	inj := &PodFloxMutator{GcrootBase: "/gc"}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"flox.seedmatic.io/nix-build.builder": "render-store"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "builder"}, {Name: "sidecar"}}},
	}
	if err := inj.Default(context.Background(), pod); err != nil {
		t.Fatalf("default: %v", err)
	}
	// The named PVC is added as a pod volume (volume name == PVC name).
	if !hasVolume(pod.Spec.Volumes, "render-store") {
		t.Fatalf("nix-store volume not added: %+v", pod.Spec.Volumes)
	}
	if pvc := pod.Spec.Volumes[0].PersistentVolumeClaim; pvc == nil || pvc.ClaimName != "render-store" {
		t.Errorf("volume is not the named PVC: %+v", pod.Spec.Volumes[0].VolumeSource)
	}
	// Only the annotated container is mounted + gets NIX_CONFIG.
	builder := pod.Spec.Containers[0]
	if len(builder.VolumeMounts) != 1 || builder.VolumeMounts[0].MountPath != nixBuildStoreMount {
		t.Errorf("builder mount = %+v (want %s)", builder.VolumeMounts, nixBuildStoreMount)
	}
	if envValue(builder.Env, "NIX_CONFIG") == "" {
		t.Error("builder missing NIX_CONFIG")
	}
	if len(pod.Spec.Containers[1].VolumeMounts) != 0 {
		t.Error("unannotated sidecar should not be mounted")
	}
}
