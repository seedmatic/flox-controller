// Package webhook holds the flox-controller admission webhooks. The pod flox-wait
// injector is the node-aware race barrier: a pod that opts into a flox env must not start
// its (flox-injected) containers until that env's GC-root exists on the node it landed on.
package webhook

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/seedmatic/flox-controller/internal/floxenv"
)

const (
	// floxEnvAnnotationPrefix is the per-container opt-in the NRI plugin keys on:
	// flox.dev/environment.<container> = "<category>/<name>". We mirror it to know which
	// GC-roots a pod will need before its containers create.
	floxEnvAnnotationPrefix = "flox.dev/environment."
	defaultCategory         = "networking" // matches the plugin's bare-name fallback

	floxWaitContainerName = "flox-wait"
	floxWaitVolumeName    = "flox-gcroots"
)

// PodFloxWaitInjector is a mutating webhook (CustomDefaulter) that prepends a "flox-wait"
// init container to any pod bearing flox.dev/environment.<c> annotations. The init container
// hostPath-mounts the GC-root base read-only and blocks until every referenced GC-root
// (<base>/<category>/<name>) exists on the node — so the flox-controller has realised the env
// there before the flox-injected containers start. It is NOT itself flox-annotated, so the
// NRI plugin ignores it (plain shell, no /nix overlay). Node-aware (runs on the assigned
// node) and can block far longer than an NRI hook or a scheduling gate could.
type PodFloxWaitInjector struct {
	// GcrootBase is the flox-runtime GC-root dir on the node (matches the controller's
	// --gcroot-base and the NRI plugin's floxEnvGcrootBase).
	GcrootBase string
	// WaitImage is the tiny image the init container runs (needs /bin/sh); e.g. busybox.
	WaitImage string
	// TimeoutSeconds bounds the wait; on expiry the init container fails so the stall is
	// visible (CrashLoopBackOff) rather than hanging forever.
	TimeoutSeconds int
	// TokenSecretName/TokenSecretKey locate the FloxHub token (a replicated Secret present
	// in the pod's namespace). When set, FLOX_FLOXHUB_TOKEN is injected valueFrom that key
	// into every flox-annotated container. Empty disables token injection (the knobs still go in).
	TokenSecretName string
	TokenSecretKey  string
}

// Default injects the flox-wait init container. Idempotent: a pod that already carries it
// (e.g. a re-admission) is left unchanged.
func (i *PodFloxWaitInjector) Default(_ context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected *corev1.Pod, got %T", obj)
	}

	gcroots := gcrootsFromAnnotations(pod.Annotations, i.GcrootBase)
	if len(gcroots) == 0 {
		return nil // pod opted into no flox env
	}

	// Inject the canonical flox settings (+ the FloxHub token) into every flox-annotated
	// container — the single vector, subsuming the NRI plugin's AddEnv and the flox-runtime
	// ConfigMap envFrom. Idempotent (upsert): a var already set on the container wins.
	i.injectFloxEnv(pod)

	for _, c := range pod.Spec.InitContainers {
		if c.Name == floxWaitContainerName {
			return nil // already injected
		}
	}

	timeout := i.TimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}
	image := i.WaitImage
	if image == "" {
		image = "busybox:stable"
	}

	pod.Spec.InitContainers = append([]corev1.Container{{
		Name:    floxWaitContainerName,
		Image:   image,
		Command: []string{"/bin/sh", "-c", waitScript(gcroots, timeout)},
		VolumeMounts: []corev1.VolumeMount{{
			Name:      floxWaitVolumeName,
			MountPath: i.GcrootBase,
			ReadOnly:  true,
		}},
	}}, pod.Spec.InitContainers...)

	if !hasVolume(pod.Spec.Volumes, floxWaitVolumeName) {
		hostPathDir := corev1.HostPathDirectoryOrCreate
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: floxWaitVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: i.GcrootBase, Type: &hostPathDir},
			},
		})
	}
	return nil
}

// injectFloxEnv upserts the canonical flox settings (floxenv) — and, when a token secret is
// configured, FLOX_FLOXHUB_TOKEN valueFrom that Secret — onto every container that opted into a
// flox env via a per-container flox.dev/environment.<container> annotation. Init containers (incl.
// the flox-wait busybox) are untouched: they are not flox-activated.
func (i *PodFloxWaitInjector) injectFloxEnv(pod *corev1.Pod) {
	annotated := map[string]struct{}{}
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, floxEnvAnnotationPrefix) && v != "" {
			annotated[strings.TrimPrefix(k, floxEnvAnnotationPrefix)] = struct{}{}
		}
	}
	for idx := range pod.Spec.Containers {
		c := &pod.Spec.Containers[idx]
		if _, ok := annotated[c.Name]; !ok {
			continue
		}
		for _, s := range floxenv.Settings() {
			upsertEnv(c, corev1.EnvVar{Name: s.Name, Value: s.Value})
		}
		if i.TokenSecretName != "" && i.TokenSecretKey != "" {
			upsertEnv(c, corev1.EnvVar{
				Name: "FLOX_FLOXHUB_TOKEN",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: i.TokenSecretName},
						Key:                  i.TokenSecretKey,
					},
				},
			})
		}
	}
}

// upsertEnv sets env var v on the container unless a var of that name is already present — an
// explicit value on the pod spec wins over the injected default.
func upsertEnv(c *corev1.Container, v corev1.EnvVar) {
	for _, existing := range c.Env {
		if existing.Name == v.Name {
			return
		}
	}
	c.Env = append(c.Env, v)
}

// gcrootsFromAnnotations resolves every flox.dev/environment.<c> annotation to the GC-root
// path the NRI plugin will readlink: <base>/<category>/<name>. Sorted + de-duplicated so the
// injected script is stable across admissions.
func gcrootsFromAnnotations(annotations map[string]string, base string) []string {
	seen := map[string]struct{}{}
	for k, v := range annotations {
		if !strings.HasPrefix(k, floxEnvAnnotationPrefix) || v == "" {
			continue
		}
		category, name := defaultCategory, v
		if parts := strings.SplitN(v, "/", 2); len(parts) == 2 {
			category, name = parts[0], parts[1]
		}
		seen[filepath.Join(base, category, name)] = struct{}{}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

func waitScript(gcroots []string, timeoutSeconds int) string {
	maxIters := timeoutSeconds / 2
	if maxIters < 1 {
		maxIters = 1
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, p := range gcroots {
		fmt.Fprintf(&b,
			"i=0; until [ -e '%s' ]; do i=$((i+1)); "+
				"if [ \"$i\" -gt %d ]; then echo 'flox-wait: timed out waiting for %s'; exit 1; fi; "+
				"echo 'flox-wait: waiting for %s'; sleep 2; done\n",
			p, maxIters, p, p)
	}
	b.WriteString("echo 'flox-wait: all gcroots present'\n")
	return b.String()
}

func hasVolume(volumes []corev1.Volume, name string) bool {
	for _, v := range volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// SetupWebhookWithManager registers the pod-mutating webhook. Only wired when the controller
// runs with --enable-webhook (the cluster-manager Deployment), never in the node-agent
// DaemonSet — serving the webhook requires TLS certs the DaemonSet has no reason to carry.
func (i *PodFloxWaitInjector) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(i).
		Complete()
}
