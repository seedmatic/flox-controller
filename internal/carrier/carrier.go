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

// EnsureBase creates the base carrier FloxEnv in namespace if absent — a DEFAULT that
// never clobbers an operator's override. The normal reconcile loop then containerizes
// it and imports the carrier into the node's containerd.
func EnsureBase(ctx context.Context, c client.Client, namespace string) error {
	var env floxv1alpha1.FloxEnv
	if err := yaml.Unmarshal(baseCarrierYAML, &env); err != nil {
		return fmt.Errorf("parse embedded base carrier: %w", err)
	}
	env.Namespace = namespace

	var existing floxv1alpha1.FloxEnv
	err := c.Get(ctx, client.ObjectKeyFromObject(&env), &existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := c.Create(ctx, &env); err != nil {
			return fmt.Errorf("create base carrier: %w", err)
		}
		log.FromContext(ctx).Info("ensured base carrier FloxEnv",
			"namespace", namespace, "name", env.Name)
		return nil
	case err != nil:
		return fmt.Errorf("get base carrier: %w", err)
	default:
		// Already present — leave the operator's version untouched.
		return nil
	}
}
