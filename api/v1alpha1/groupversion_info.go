// Package v1alpha1 contains the FloxEnv API (group flox.seedmatic.io).
// +kubebuilder:object:generate=true
// +groupName=flox.seedmatic.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group/version for the FloxEnv API.
	GroupVersion = schema.GroupVersion{Group: "flox.seedmatic.io", Version: "v1alpha1"}

	// SchemeBuilder registers the API types into a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the FloxEnv types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
