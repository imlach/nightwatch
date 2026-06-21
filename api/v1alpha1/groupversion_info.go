// Package v1alpha1 holds the ElasticNode API types - the desired-power-state CRD
// Nightwatch reconciles. The CR lives on the management cluster; the nodes it
// describes live on a separate target cluster.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version for the Nightwatch API.
var GroupVersion = schema.GroupVersion{Group: "nightwatch.imla.ch", Version: "v1alpha1"}

// SchemeBuilder registers the API types into a runtime.Scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the API types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&ElasticNode{}, &ElasticNodeList{})
}
