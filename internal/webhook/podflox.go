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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/seedmatic/flox-controller/internal/floxenv"
)

const (
	// floxEnvAnnotationPrefix is the per-container opt-in the NRI plugin keys on:
	// flox.seedmatic.io/environment.<container> = "<category>/<name>". We mirror it to know which
	// GC-roots a pod will need before its containers create.
	floxEnvAnnotationPrefix = "flox.seedmatic.io/environment."
	defaultCategory         = "networking" // matches the plugin's bare-name fallback

	floxWaitContainerName = "flox-wait"
	floxWaitVolumeName    = "flox-gcroots"

	// nixBuildAnnotationPrefix is the per-container opt-in for the nix-build capability:
	// flox.seedmatic.io/nix-build.<container> = "<pvc-name>". The VALUE is the PVC the step assigns
	// as its persistent nix store (reused across the step's task runs = a warm cache; distinct steps
	// name distinct PVCs = isolated stores). We ensure that PVC (create-if-absent) + inject it as a
	// volume mounted at nixBuildStoreMount + NIX_CONFIG. The NRI plugin reads the same annotation and
	// hosts the /nix store overlay's upper/work on nixBuildStoreMount.
	nixBuildAnnotationPrefix = "flox.seedmatic.io/nix-build."
	// nixBuildStoreMount is the container-absolute path where the assigned nix-store PVC is mounted —
	// the shared contract between this webhook and the NRI plugin's overlay upper_backing (a sibling
	// of the gcroot-base constant). Internal, not user-facing.
	nixBuildStoreMount = "/var/lib/flox-nri/nix-build-store"
)

// PodFloxMutator is a mutating webhook (CustomDefaulter) that prepends a "flox-wait"
// init container to any pod bearing flox.seedmatic.io/environment.<c> annotations. The init container
// hostPath-mounts the GC-root base read-only and blocks until every referenced GC-root
// (<base>/<category>/<name>) exists on the node — so the flox-controller has realised the env
// there before the flox-injected containers start. It is NOT itself flox-annotated, so the
// NRI plugin ignores it (plain shell, no /nix overlay). Node-aware (runs on the assigned
// node) and can block far longer than an NRI hook or a scheduling gate could.
type PodFloxMutator struct {
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

	// Client ensures the nix-build store PVC (named by the pod's annotation) exists
	// (create-if-absent) when a pod opts into the nix-build capability. nil disables the ensure (the
	// volume is still injected — the PVC must then pre-exist).
	Client client.Client
	// NixStoreClass/NixStoreSize size the ensured PVC (its name comes from the annotation value, the
	// step-assigned store reused across the step's task runs = a warm nix store cache).
	NixStoreClass string
	NixStoreSize  string
}

// Default is the pod mutator entry point. It dispatches the independent flox concerns; each is a
// no-op unless its own per-container annotation opted in, so a pod can request any subset.
func (i *PodFloxMutator) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected *corev1.Pod, got %T", obj)
	}
	i.injectFloxWait(pod)
	return i.injectNixBuild(ctx, pod)
}

// injectFloxWait handles the flox env concern: it prepends the node-aware "flox-wait" init
// container + upserts the canonical flox settings/token onto every flox-annotated container.
// No-op for a pod that opted into no flox env. Idempotent (a re-admission leaves it unchanged).
func (i *PodFloxMutator) injectFloxWait(pod *corev1.Pod) {
	gcroots := gcrootsFromAnnotations(pod.Annotations, i.GcrootBase)
	if len(gcroots) == 0 {
		return // pod opted into no flox env
	}

	// Inject the canonical flox settings (+ the FloxHub token) into every flox-annotated
	// container — the single vector, subsuming the NRI plugin's AddEnv and the flox-runtime
	// ConfigMap envFrom. Idempotent (upsert): a var already set on the container wins.
	i.injectFloxEnv(pod)

	for _, c := range pod.Spec.InitContainers {
		if c.Name == floxWaitContainerName {
			return // already injected
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
}

// injectNixBuild handles the nix-build capability: for every container bearing
// flox.seedmatic.io/nix-build.<c>=<mountPath>, it ensures the per-namespace persistent nix-store
// PVC exists, injects it as a shared pod volume mounted at <mountPath> on that container, and
// upserts NIX_CONFIG. The NRI plugin reads the same annotation and hosts the /nix store overlay's
// upper/work on <mountPath>, so the persistent PVC becomes the warm store reused across renders.
// No-op for a pod that opted into no nix-build.
func (i *PodFloxMutator) injectNixBuild(ctx context.Context, pod *corev1.Pod) error {
	targets := map[string]string{} // container name -> mount path
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, nixBuildAnnotationPrefix) && v != "" {
			targets[strings.TrimPrefix(k, nixBuildAnnotationPrefix)] = v
		}
	}
	if len(targets) == 0 {
		return nil
	}

	// Each distinct PVC named across the pod's nix-build containers: ensure it exists + add it as a
	// pod volume once. The volume name == the PVC name (a DNS-1123 label, valid as a volume name).
	ensured := map[string]struct{}{}
	for _, pvcName := range targets {
		if _, done := ensured[pvcName]; done {
			continue
		}
		ensured[pvcName] = struct{}{}
		if err := i.ensureNixStorePVC(ctx, pod.Namespace, pvcName); err != nil {
			return err
		}
		if !hasVolume(pod.Spec.Volumes, pvcName) {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: pvcName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				},
			})
		}
	}

	for idx := range pod.Spec.InitContainers {
		if pvc, ok := targets[pod.Spec.InitContainers[idx].Name]; ok {
			mountNixStore(&pod.Spec.InitContainers[idx], pvc)
		}
	}
	for idx := range pod.Spec.Containers {
		if pvc, ok := targets[pod.Spec.Containers[idx].Name]; ok {
			mountNixStore(&pod.Spec.Containers[idx], pvc)
		}
	}
	return nil
}

// mountNixStore mounts the assigned nix-store volume (named for its PVC) at nixBuildStoreMount and
// upserts NIX_CONFIG onto c. Idempotent: skips the mount if the container already carries it.
func mountNixStore(c *corev1.Container, volumeName string) {
	present := false
	for _, m := range c.VolumeMounts {
		if m.Name == volumeName {
			present = true
			break
		}
	}
	if !present {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: volumeName, MountPath: nixBuildStoreMount})
	}
	upsertEnv(c, corev1.EnvVar{Name: "NIX_CONFIG", Value: floxenv.NixConfig()})
}

// ensureNixStorePVC create-if-absents the named persistent nix-store PVC in namespace. Idempotent:
// AlreadyExists is success. A nil Client skips the ensure (the PVC must then pre-exist).
func (i *PodFloxMutator) ensureNixStorePVC(ctx context.Context, namespace, name string) error {
	if i.Client == nil {
		return nil
	}
	size := i.NixStoreSize
	if size == "" {
		size = "30Gi"
	}
	var storageClass *string
	if i.NixStoreClass != "" {
		storageClass = &i.NixStoreClass
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: storageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
	if err := i.Client.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("ensure nix-store PVC %s/%s: %w", namespace, name, err)
	}
	return nil
}

// injectFloxEnv upserts the canonical flox settings (floxenv) — and, when a token secret is
// configured, FLOX_FLOXHUB_TOKEN valueFrom that Secret — onto every container that opted into a
// flox env via a per-container flox.seedmatic.io/environment.<container> annotation. A flox.seedmatic.io/environment.<c>
// annotation may name an INIT container (e.g. headscale's config-init / wait-for-headscale, which run
// `flox activate` to render config or wait for a barrier): the NRI plugin puts flox on their PATH, so
// they need the same knobs + token. Our own flox-wait busybox carries no such annotation and is
// therefore skipped.
func (i *PodFloxMutator) injectFloxEnv(pod *corev1.Pod) {
	annotated := map[string]struct{}{}
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, floxEnvAnnotationPrefix) && v != "" {
			annotated[strings.TrimPrefix(k, floxEnvAnnotationPrefix)] = struct{}{}
		}
	}
	for idx := range pod.Spec.InitContainers {
		i.injectFloxEnvInto(&pod.Spec.InitContainers[idx], annotated)
	}
	for idx := range pod.Spec.Containers {
		i.injectFloxEnvInto(&pod.Spec.Containers[idx], annotated)
	}
}

// injectFloxEnvInto upserts the flox knobs (+ optional token) onto c iff c opted into a flox env
// (its name appears in the flox.seedmatic.io/environment.<c> annotation set).
func (i *PodFloxMutator) injectFloxEnvInto(c *corev1.Container, annotated map[string]struct{}) {
	if _, ok := annotated[c.Name]; !ok {
		return
	}
	for _, s := range floxenv.Settings() {
		upsertEnv(c, corev1.EnvVar{Name: s.Name, Value: s.Value})
	}
	if i.TokenSecretName != "" && i.TokenSecretKey != "" {
		// optional: the webhook injects cluster-wide, but the (replicated) token secret only
		// exists in namespaces that created its replicate-from stub — a namespace without it
		// must NOT wedge on a missing secret; flox there runs unauthenticated (warnings only).
		optional := true
		upsertEnv(c, corev1.EnvVar{
			Name: "FLOX_FLOXHUB_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: i.TokenSecretName},
					Key:                  i.TokenSecretKey,
					Optional:             &optional,
				},
			},
		})
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

// gcrootsFromAnnotations resolves every flox.seedmatic.io/environment.<c> annotation to the GC-root
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
		// Test -L (the gcroot SYMLINK exists), not -e: the gcroot is a nix gcroot symlink into
		// /nix/store, which this init container does NOT mount (only the gcroot base is), so -e
		// would follow the symlink to an unreachable target and never succeed. The symlink's
		// PRESENCE is the barrier signal — the controller placed it; the NRI plugin resolves the
		// target later (it has /nix). Keep -e as a fallback for a non-symlink gcroot.
		fmt.Fprintf(&b,
			"i=0; until [ -L '%s' ] || [ -e '%s' ]; do i=$((i+1)); "+
				"if [ \"$i\" -gt %d ]; then echo 'flox-wait: timed out waiting for %s'; exit 1; fi; "+
				"echo 'flox-wait: waiting for %s'; sleep 2; done\n",
			p, p, maxIters, p, p)
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

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;create

// SetupWebhookWithManager registers the pod-mutating webhook. Only wired when the controller
// runs with --enable-webhook (the cluster-manager Deployment), never in the node-agent
// DaemonSet — serving the webhook requires TLS certs the DaemonSet has no reason to carry.
func (i *PodFloxMutator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(i).
		Complete()
}
