package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// UpdatePolicy directs how/when the lock is refreshed. The lock is NEVER produced by
// the controller (no per-node re-locking → determinism); it is produced by the
// operator's env-bumper per this policy.
type UpdatePolicy struct {
	// Mode is Manual (the lock changes only via an explicit operator bump) or Track
	// (the env-bumper may refresh the lock within the packages' constraints).
	// +kubebuilder:validation:Enum=Manual;Track
	// +kubebuilder:default=Manual
	// +optional
	Mode string `json:"mode,omitempty"`
}

// FloxEnvSpec is the maintainer-authored INTENT. It never carries generated artifacts
// (the lock, resolved paths) — those live in status.
type FloxEnvSpec struct {
	// Consumption is how the env is used: overlaid into a pod at container-create
	// (`overlay`) or containerized into a carrier base image (`image`).
	// +kubebuilder:validation:Enum=overlay;image
	// +kubebuilder:default=overlay
	// +optional
	Consumption string `json:"consumption,omitempty"`

	// Manifest is the flox manifest expressed as YAML — a faithful TRANSPOSITION of
	// `manifest.toml` (same structure, NOT a re-modelled schema): `install`, `include`,
	// `vars`, `hook`, `profile`, `options`, `services`. The controller serialises it to
	// `manifest.toml`. Composition stays flox-native: `include` uses PATHS that resolve
	// to other envs' host folders (each env materialises at its `folder`), so the CR
	// does NOT model includes separately (no CR↔manifest conflict).
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	Manifest *runtime.RawExtension `json:"manifest,omitempty"`

	// Update directs lock refresh (see UpdatePolicy).
	// +optional
	Update *UpdatePolicy `json:"update,omitempty"`

	// Folder is the relative host-layout path the env materialises in
	// (`<envroot>/<folder>/<name>/`), DISSOCIATED from the k8s namespace and MAY be a
	// structured path (e.g. "mesh/base"), unlike a namespace. It is what other envs'
	// manifest `include` paths resolve against. Defaults to metadata.namespace when empty.
	// +optional
	Folder string `json:"folder,omitempty"`

	// Inject declares env vars the flox-controller's pod webhook adds to every container that opts
	// into THIS env via a flox.seedmatic.io/environment.<c> annotation — how a FloxEnv contributes
	// REQUIRED runtime env/secrets to its consumers (e.g. the git-sops env contributes SOPS_AGE_KEY
	// from the sops-age Secret), so a consumer only annotates the env and never wires the env's
	// secrets itself. A referenced Secret must exist in the consumer pod's namespace (replicate it
	// there); injection is secretKeyRef (kubelet resolves it), the controller reads no secret.
	// +optional
	Inject []InjectedEnv `json:"inject,omitempty"`
}

// InjectedEnv is one env var a FloxEnv contributes to its consumers (see FloxEnvSpec.Inject).
// Exactly one of Value / SecretKeyRef is set.
type InjectedEnv struct {
	// Name is the env var name (e.g. SOPS_AGE_KEY).
	Name string `json:"name"`

	// Value is a literal value.
	// +optional
	Value string `json:"value,omitempty"`

	// SecretKeyRef injects valueFrom a Secret key in the consumer pod's namespace.
	// +optional
	SecretKeyRef *InjectedSecretKeyRef `json:"secretKeyRef,omitempty"`
}

// InjectedSecretKeyRef selects a key of a Secret resolved in the consumer pod's namespace.
type InjectedSecretKeyRef struct {
	// Name is the Secret name.
	Name string `json:"name"`

	// Key is the key within the Secret's data.
	Key string `json:"key"`

	// Optional, when true, lets the container start even if the Secret/key is absent (the env is
	// then unset) — mirrors core/v1 secretKeyRef.optional. Defaults false.
	// +optional
	Optional *bool `json:"optional,omitempty"`
}

// NodeRealization reports one node's realisation of the env.
type NodeRealization struct {
	// Node is the node name.
	Node string `json:"node"`

	// Ready is true once the closures + GC-roots are present on the node.
	Ready bool `json:"ready"`

	// StorePath is the realised env store path the NRI plugin resolves to.
	// +optional
	StorePath string `json:"storePath,omitempty"`

	// EnvPath is the .flox source dir on the node (<env-root>/<folder>/<name>) — where the
	// operator can cd to read the manifest/lock/run. Inspection aid, not a contract.
	// +optional
	EnvPath string `json:"envPath,omitempty"`

	// GcrootPath is the flox-runtime GC-root on the node the NRI plugin readlinks to resolve
	// this env (<gcroot-base>/<folder>/<name>) — the producer↔plugin handoff. Inspection aid.
	// +optional
	GcrootPath string `json:"gcrootPath,omitempty"`

	// ObservedGeneration is the FloxEnv generation this realisation reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// FloxEnvStatus is GENERATED — never hand-edited. It holds the deterministic lock, what
// each package resolved to (inspectability), and per-node realisation.
type FloxEnvStatus struct {
	// Lock is the resolved flox manifest.lock (the deterministic pin) — GENERATED, in
	// status so the maintenance loop is introspect(status) → adjust(spec); you never
	// hand-edit a lock. Produced by a lock-producer (the operator env-bumper, or an
	// in-cluster lock-controller reacting to spec.update — OPEN axis, gated on avoiding
	// FloxHub), NEVER by the node-agent, which realises it verbatim and never re-locks.
	// +optional
	Lock string `json:"lock,omitempty"`

	// RelockToken echoes the last honored value of the flox.seedmatic.io/relock annotation. When
	// the annotation's value differs from this, the reconciler drops status.Lock so the next realise
	// re-locks from scratch (pulls fresh flake inputs) instead of re-using the pin — then records the
	// value here, so a given force fires exactly once per distinct annotation value. Patch the
	// annotation (e.g. to a timestamp) to force a re-lock without editing spec or restarting.
	// +optional
	RelockToken string `json:"relockToken,omitempty"`

	// Resolved reports what each package resolved to, for inspection.
	// +optional
	Resolved map[string]string `json:"resolved,omitempty"`

	// Realized reports per-node realisation.
	// +optional
	// +listType=map
	// +listMapKey=node
	Realized []NodeRealization `json:"realized,omitempty"`

	// Conditions is the standard condition set.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fenv
// +kubebuilder:printcolumn:name="Consumption",type=string,JSONPath=`.spec.consumption`
// +kubebuilder:printcolumn:name="Folder",type=string,JSONPath=`.spec.folder`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FloxEnv is a flox environment as a first-class Kubernetes resource.
type FloxEnv struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FloxEnvSpec   `json:"spec,omitempty"`
	Status FloxEnvStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FloxEnvList contains a list of FloxEnv.
type FloxEnvList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FloxEnv `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FloxEnv{}, &FloxEnvList{})
}
