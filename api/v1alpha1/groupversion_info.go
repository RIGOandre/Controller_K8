// Package v1alpha1 contém os tipos da API preview.rigo.dev/v1alpha1.
//
// +kubebuilder:object:generate=true
// +groupName=preview.rigo.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion é o group/version registrado por estes tipos.
	GroupVersion = schema.GroupVersion{Group: "preview.rigo.dev", Version: "v1alpha1"}

	// SchemeBuilder junta os tipos Go ao scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registra os tipos deste group/version num scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
