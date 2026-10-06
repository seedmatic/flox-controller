// Package webhook holds the flox-controller admission webhooks. The pod flox mutator is the
// node-aware race barrier: a pod that opts into a flox env is admitted with a scheduling gate so
// it stays unscheduled (no container starts) until the controller's gate reconciler observes the
// env realised at the current generation and removes the gate.
package webhook

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/floxenv"
)

const (
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

// PodFloxMutator is a mutating webhook (CustomDefaulter) that adds a scheduling gate to any pod
// bearing flox.seedmatic.io/environment.<c> annotations, so the pod stays SchedulingGated
// (unscheduled, no container starts) until the controller's gate reconciler removes the gate —
// which it does once every referenced FloxEnv is realised at the pod's current generation. The
// pod itself needs no node access and no API access: the controller owns both the realisation
// and the ungate. It also upserts the canonical flox settings/token onto the annotated containers.
type PodFloxMutator struct {
	// TokenSecretName/TokenSecretKey locate the FloxHub token (a replicated Secret present
	// in the pod's namespace). When set, FLOX_FLOXHUB_TOKEN is injected valueFrom that key
	// into every flox-annotated container. Empty disables token injection (the knobs still go in).
	TokenSecretName string
	TokenSecretKey  string

	// Client ensures the nix-build store PVC (named by the pod's annotation) exists
	// (create-if-absent) when a pod opts into the nix-build capability, and READS the FloxEnv CR a
	// container opts into to apply its spec.inject. nil disables both (volume still injected — the
	// PVC must then pre-exist; and no per-env inject).
	Client client.Client
	// FloxEnvNamespace is where the FloxEnv CRs live (the controller's own namespace) — the webhook
	// GETs `<annotation-value name>` there to read its spec.inject. Empty disables inject.
	FloxEnvNamespace string
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
	i.injectFloxWait(ctx, pod)
	return i.injectNixBuild(ctx, pod)
}

// injectFloxWait handles the flox env concern: it adds the env-ready scheduling gate + upserts
// the canonical flox settings/token onto every flox-annotated container. No-op for a pod that
// opted into no flox env. Idempotent (a re-admission leaves it unchanged).
//
// Adding a scheduling gate is only legal at pod CREATE (the API server rejects adding one via
// update, and once the gate reconciler removes it the scheduler assigns a node). The mutating
// webhook config MUST therefore scope this to CREATE; the pod.Spec.NodeName guard below is
// defence-in-depth so a stray UPDATE admission never re-gates a pod the scheduler already placed.
func (i *PodFloxMutator) injectFloxWait(ctx context.Context, pod *corev1.Pod) {
	if len(floxenv.RefsFromAnnotations(pod.Annotations)) == 0 {
		return // pod opted into no flox env
	}

	// Inject the canonical flox settings (+ the FloxHub token, + each env's declared spec.inject)
	// into every flox-annotated container — the single vector, subsuming the NRI plugin's AddEnv and
	// the flox-runtime ConfigMap envFrom. Idempotent (upsert): a var already set on the container wins.
	i.injectFloxEnv(ctx, pod)

	if pod.Spec.NodeName != "" {
		return // already scheduled — must not (and cannot) add a gate
	}
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == floxenv.SchedulingGateName {
			return // already gated
		}
	}
	pod.Spec.SchedulingGates = append(pod.Spec.SchedulingGates,
		corev1.PodSchedulingGate{Name: floxenv.SchedulingGateName})
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
func (i *PodFloxMutator) injectFloxEnv(ctx context.Context, pod *corev1.Pod) {
	// Map each opted-in container to the env it named (annotation VALUE = "<folder>/<name>"; the
	// FloxEnv CR name is that value's last segment), then resolve each distinct env's spec.inject
	// ONCE by GETting the CR in FloxEnvNamespace.
	envOf := map[string]string{}
	for k, v := range pod.Annotations {
		if strings.HasPrefix(k, floxenv.AnnotationPrefix) && v != "" {
			envOf[strings.TrimPrefix(k, floxenv.AnnotationPrefix)] = envName(v)
		}
	}
	injects := i.resolveInjects(ctx, envOf)
	for idx := range pod.Spec.InitContainers {
		i.injectFloxEnvInto(&pod.Spec.InitContainers[idx], envOf, injects)
	}
	for idx := range pod.Spec.Containers {
		i.injectFloxEnvInto(&pod.Spec.Containers[idx], envOf, injects)
	}
}

// resolveInjects GETs each distinct FloxEnv named in envOf (from FloxEnvNamespace) and returns its
// spec.inject, keyed by env name. A missing env / absent client is a nil entry (no inject), never a
// hard failure — a pod must still admit if an env's CR is momentarily unreadable.
func (i *PodFloxMutator) resolveInjects(
	ctx context.Context, envOf map[string]string,
) map[string][]floxv1alpha1.InjectedEnv {
	out := map[string][]floxv1alpha1.InjectedEnv{}
	if i.Client == nil || i.FloxEnvNamespace == "" {
		return out
	}
	for _, name := range envOf {
		if _, done := out[name]; done {
			continue
		}
		var env floxv1alpha1.FloxEnv
		if err := i.Client.Get(
			ctx, client.ObjectKey{Namespace: i.FloxEnvNamespace, Name: name}, &env); err != nil {
			if !apierrors.IsNotFound(err) {
				log.FromContext(ctx).Error(err, "flox inject: GET FloxEnv failed", "name", name)
			}
			out[name] = nil
			continue
		}
		out[name] = env.Spec.Inject
	}
	return out
}

// injectFloxEnvInto upserts the flox knobs (+ optional FloxHub token, + the container's env's
// declared spec.inject) onto c iff c opted into a flox env (its name is a key of envOf).
func (i *PodFloxMutator) injectFloxEnvInto(
	c *corev1.Container, envOf map[string]string, injects map[string][]floxv1alpha1.InjectedEnv,
) {
	name, ok := envOf[c.Name]
	if !ok {
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
	// The env's own contributions (e.g. git-sops → SOPS_AGE_KEY). upsert: an explicit value on the
	// pod spec still wins.
	for _, inj := range injects[name] {
		upsertEnv(c, injectedEnvVar(inj))
	}
}

// envName is the FloxEnv CR name an environment.<c> annotation VALUE ("<folder>/<name>") points at
// — the segment after the last '/'.
func envName(value string) string {
	if i := strings.LastIndex(value, "/"); i >= 0 {
		return value[i+1:]
	}
	return value
}

// injectedEnvVar renders a FloxEnv spec.inject entry as a container EnvVar (secretKeyRef or literal).
func injectedEnvVar(inj floxv1alpha1.InjectedEnv) corev1.EnvVar {
	if inj.SecretKeyRef != nil {
		return corev1.EnvVar{
			Name: inj.Name,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: inj.SecretKeyRef.Name},
					Key:                  inj.SecretKeyRef.Key,
					Optional:             inj.SecretKeyRef.Optional,
				},
			},
		}
	}
	return corev1.EnvVar{Name: inj.Name, Value: inj.Value}
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
