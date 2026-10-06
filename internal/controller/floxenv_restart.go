package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/floxenv"
)

// restartMarker is stamped on a consuming workload's POD TEMPLATE with the digest of the env's
// realized lock. Changing a pod-template annotation rolls the workload natively (the template hash
// changes → new ReplicaSet / rolling update), so the fresh pods re-realize the NEW binary through
// the NRI plugin. The value is DETERMINISTIC (the lock digest), which is what makes the restart
// idempotent: a redundant node-agent recomputes the same digest and the MergeFrom patch is a no-op.
const restartMarker = "flox.seedmatic.io/restarted-for-lock"

// restartConsumers rolls every workload that consumes this env so it adopts the freshly realized
// binary. WHY: the NRI plugin resolves the env at CONTAINER START, so a re-lock alone leaves a
// running pod on the OLD closure until it restarts — this closes that loop. Called only when the
// realized lock actually CHANGED (see the caller's guard), so unchanged re-realises cost nothing;
// and it stamps a deterministic digest, so it is safe to run redundantly across the node-agents.
//
// Consumers are matched by the same per-container `flox.seedmatic.io/environment.<c>` annotation the
// webhook/NRI plugin key on (floxenv.RefsFromAnnotations), read off the POD TEMPLATE — so a
// Deployment/DaemonSet/StatefulSet in ANY namespace that opts into this env (folder-or-namespace +
// name) is rolled. Best-effort per workload: a single patch failure is joined and returned so the
// caller retries (the realize is a nix cache-hit on retry).
func (r *FloxEnvReconciler) restartConsumers(
	ctx context.Context, env *floxv1alpha1.FloxEnv, lock string,
) error {
	folder := env.Spec.Folder
	if folder == "" {
		folder = env.Namespace
	}
	target := floxenv.EnvRef{Folder: folder, Name: env.Name}
	digest := lockDigest(lock)

	var deployments appsv1.DeploymentList
	var daemonSets appsv1.DaemonSetList
	var statefulSets appsv1.StatefulSetList
	if err := r.List(ctx, &deployments); err != nil {
		return err
	}
	if err := r.List(ctx, &daemonSets); err != nil {
		return err
	}
	if err := r.List(ctx, &statefulSets); err != nil {
		return err
	}

	var errs []error
	for i := range deployments.Items {
		w := &deployments.Items[i]
		errs = append(errs, r.stampConsumer(ctx, w, &w.Spec.Template, target, digest))
	}
	for i := range daemonSets.Items {
		w := &daemonSets.Items[i]
		errs = append(errs, r.stampConsumer(ctx, w, &w.Spec.Template, target, digest))
	}
	for i := range statefulSets.Items {
		w := &statefulSets.Items[i]
		errs = append(errs, r.stampConsumer(ctx, w, &w.Spec.Template, target, digest))
	}
	return errors.Join(errs...)
}

// stampConsumer rolls one workload IFF its pod template consumes target and is not already stamped
// with digest — a MergeFrom patch of the template annotation, idempotent by the deterministic value.
func (r *FloxEnvReconciler) stampConsumer(
	ctx context.Context,
	workload client.Object,
	template *corev1.PodTemplateSpec,
	target floxenv.EnvRef,
	digest string,
) error {
	if !slices.Contains(floxenv.RefsFromAnnotations(template.Annotations), target) {
		return nil
	}
	if template.Annotations[restartMarker] == digest {
		return nil // already rolled for this lock — no-op (idempotent across node-agents)
	}
	base := workload.DeepCopyObject().(client.Object)
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[restartMarker] = digest
	log.FromContext(ctx).Info("rolling flox-env consumer",
		"workload", client.ObjectKeyFromObject(workload), "env", target.Folder+"/"+target.Name)
	return r.Patch(ctx, workload, client.MergeFrom(base))
}

// lockDigest is a short, stable fingerprint of the realized lock — the pod-template value whose
// change triggers (and whose sameness suppresses) a consumer roll.
func lockDigest(lock string) string {
	sum := sha256.Sum256([]byte(lock))
	return hex.EncodeToString(sum[:6])
}
