package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SourceReference points at a Flux source (a source.toolkit.fluxcd.io object) whose
// reconciled artifact carries the flake tree. The controller reads the referenced
// object's status.artifact to obtain an IN-CLUSTER tarball at the EXACT reconciled
// revision — no external fetch, no token (Flux already fetched it, with its own auth).
type SourceReference struct {
	// Kind of the Flux source. Only GitRepository is supported today.
	// +kubebuilder:validation:Enum=GitRepository
	// +kubebuilder:default=GitRepository
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the Flux source object.
	Name string `json:"name"`

	// Namespace of the Flux source object. Defaults to the FloxFlake's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// FloxFlakeSpec binds a nix flake (the workload-package catalog) to a Flux source.
// A FloxEnv references it via `install.<id>.flake = "<floxflake-name>#<output>"`; the
// controller rewrites that to the concrete nix ref derived from the source's artifact.
// This is the source/consumer split (à la Flux Kustomization -> GitRepository): the
// controller stays generic — it knows nothing about the workload, only how to resolve
// a flake output from a Flux source.
type FloxFlakeSpec struct {
	// SourceRef is the Flux source whose reconciled artifact carries the flake tree.
	SourceRef SourceReference `json:"sourceRef"`

	// Dir is the flake root within the source tree (the subdir holding flake.nix),
	// e.g. "runtime/flox". Empty means the source tree root.
	// +optional
	Dir string `json:"dir,omitempty"`
}

// FloxFlakeStatus reports the concrete artifact the controller has pinned the flake to.
type FloxFlakeStatus struct {
	// Revision is the source's reconciled revision the flake is pinned to
	// (e.g. "manifests/bioskop-mgmt@sha1:42a4846…").
	// +optional
	Revision string `json:"revision,omitempty"`

	// ArtifactURL is the in-cluster tarball URL (Flux source-controller) the flake is
	// resolved from at this revision.
	// +optional
	ArtifactURL string `json:"artifactURL,omitempty"`

	// FlakeRef is the concrete nix flake reference the controller derives for this
	// artifact (e.g. "tarball+http://…/<sha>.tar.gz?dir=runtime/flox"), reused by every
	// FloxEnv that references this FloxFlake.
	// +optional
	FlakeRef string `json:"flakeRef,omitempty"`

	// Conditions is the standard condition set (Ready once an artifact is resolved).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ffl
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
// +kubebuilder:printcolumn:name="Dir",type=string,JSONPath=`.spec.dir`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.revision`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FloxFlake binds a nix flake (a workload-package catalog) to a Flux source.
//
// Reconciliation: the controller reads the referenced GitRepository's status.artifact —
// the in-cluster tarball Flux's source-controller serves at the reconciled commit (Flux
// already fetched + authenticated it, so there is no external fetch and no token here) —
// and derives status.flakeRef = "tarball+<artifact-url>?dir=<spec.dir>", the concrete nix
// reference pinned to that EXACT commit. A FloxEnv install entry "floxflake:<name>#<output>"
// is rewritten to "<flakeRef>#<output>" before flox ever sees the manifest, so many FloxEnvs
// share one pinned catalog while the controller stays generic (it only knows how to resolve
// a flake from a Flux source — nothing about the workload). A new reconciled artifact (new
// commit) re-derives status.flakeRef, keeping the flake in lock-step with the deployed
// manifests (same commit, no drift).
type FloxFlake struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FloxFlakeSpec   `json:"spec,omitempty"`
	Status FloxFlakeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FloxFlakeList contains a list of FloxFlake.
type FloxFlakeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FloxFlake `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FloxFlake{}, &FloxFlakeList{})
}
