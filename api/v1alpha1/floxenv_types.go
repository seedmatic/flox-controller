package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FloxEnvRef references another FloxEnv CR, for flox `[include]` composition.
type FloxEnvRef struct {
	// Namespace of the referenced FloxEnv; defaults to the referrer's namespace when empty.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Name of the referenced FloxEnv.
	Name string `json:"name"`
}

// FloxEnvSpec is the desired flox environment. It carries the committed intent
// verbatim — the controller realises from it and never re-locks.
type FloxEnvSpec struct {
	// Manifest is the flox `manifest.toml` content (the env definition).
	Manifest string `json:"manifest"`

	// Lock is the committed flox `manifest.lock` content (pins the closures).
	Lock string `json:"lock"`

	// Folder is the relative host-layout path the env is materialised in
	// (`<envroot>/<folder>/<name>/`), DISSOCIATED from the k8s namespace so flox
	// `[include]` composition is layout-controlled. Defaults to metadata.namespace
	// when empty.
	// +optional
	Folder string `json:"folder,omitempty"`

	// Includes are other FloxEnv CRs composed via flox `[include]`. The controller
	// resolves each target's Folder to render the relative include dir.
	// +optional
	Includes []FloxEnvRef `json:"includes,omitempty"`
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

	// ObservedGeneration is the FloxEnv generation this realisation reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// FloxEnvStatus is the observed realisation across nodes.
type FloxEnvStatus struct {
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
// +kubebuilder:printcolumn:name="Folder",type=string,JSONPath=`.spec.folder`
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
