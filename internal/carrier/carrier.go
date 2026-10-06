// Package carrier embeds the base flox carrier FloxEnv — the controller's canonical
// carrier-contract provider — and ensures it on startup. The base carrier belongs to
// the controller, not the consumer: the baked controller self-provisions it, so
// carriers exist for flox pods without the consumer having to synthesise a carrier CR.
// Consumers add workload FloxEnvs (and any specialised carriers) as their own CRs.
package carrier

import (
	"context"
	_ "embed"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
)

//go:embed base-carrier.yaml
var baseCarrierYAML []byte

// managedByLabel marks the base carrier as owned by the controller. On startup EnsureBase
// updates a still-marked CR to the embedded spec (so a new controller version's carrier
// definition propagates); an operator who forks it by removing the label is left untouched.
const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "flox-controller"
)

// EnsureBase reconciles the embedded base carrier FloxEnv in namespace on startup: create it if
// absent, or update its spec to the embedded definition if the controller still owns it (the
// managed-by label). It never clobbers an operator override (label removed → left as-is).
func EnsureBase(ctx context.Context, c client.Client, namespace string) error {
	var desired floxv1alpha1.FloxEnv
	if err := yaml.Unmarshal(baseCarrierYAML, &desired); err != nil {
		return fmt.Errorf("parse embedded base carrier: %w", err)
	}
	desired.Namespace = namespace
	if desired.Labels == nil {
		desired.Labels = map[string]string{}
	}
	desired.Labels[managedByLabel] = managedByValue
	log := log.FromContext(ctx)

	var existing floxv1alpha1.FloxEnv
	err := c.Get(ctx, client.ObjectKeyFromObject(&desired), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, &desired); err != nil {
			return fmt.Errorf("create base carrier: %w", err)
		}
		log.Info("created base carrier FloxEnv", "namespace", namespace, "name", desired.Name)
		return nil
	case err != nil:
		return fmt.Errorf("get base carrier: %w", err)
	default:
		if existing.Labels[managedByLabel] != managedByValue {
			// An operator forked it (removed our label) — leave their version untouched.
			return nil
		}
		existing.Spec = desired.Spec // propagate the embedded definition
		if err := c.Update(ctx, &existing); err != nil {
			return fmt.Errorf("update base carrier: %w", err)
		}
		log.Info("updated base carrier FloxEnv to embedded spec",
			"namespace", namespace, "name", desired.Name)
		return nil
	}
}
