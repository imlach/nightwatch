package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PowerState is the desired or observed power intent for a node.
// +kubebuilder:validation:Enum=On;Off
type PowerState string

const (
	// PowerOn keeps the node in service (the fail-safe default).
	PowerOn PowerState = "On"
	// PowerOff sheds the node (drained + shut down).
	PowerOff PowerState = "Off"
)

// Phase is the coarse observed lifecycle state, for observability only - never
// read back to decide what to do next (level-triggered invariant).
// +kubebuilder:validation:Enum=Ready;Draining;Blocked;Off;PoweringOn;Error
type Phase string

const (
	PhaseReady      Phase = "Ready"
	PhaseDraining   Phase = "Draining"
	PhaseBlocked    Phase = "Blocked"
	PhaseOff        Phase = "Off"
	PhasePoweringOn Phase = "PoweringOn"
	PhaseError      Phase = "Error"
)

// ElasticNodeSpec is the desired intent - the only state Nightwatch persists.
type ElasticNodeSpec struct {
	// DesiredPower is the target power state. Defaults to On (fail-safe: a node
	// whose intent is unknown stays in service).
	// +kubebuilder:default=On
	// +optional
	DesiredPower PowerState `json:"desiredPower,omitempty"`

	// DesiredReason records why the intent is what it is (e.g. "quiet-mode",
	// "ups-load-shed", "manual") - surfaced in events/status, not control logic.
	// +optional
	DesiredReason string `json:"desiredReason,omitempty"`

	// ClusterRef names the target workload cluster this node lives on, for the
	// future multi-cluster Backends provider. Empty selects the default target.
	// +optional
	ClusterRef string `json:"clusterRef,omitempty"`
}

// ElasticNodeStatus is best-effort observability written on transitions. It is
// NEVER used to decide the next action - the reconciler recovers from spec + the
// live world.
type ElasticNodeStatus struct {
	// Phase is the coarse observed lifecycle state.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ObservedPower is the last power state read from the BMC.
	// +optional
	ObservedPower PowerState `json:"observedPower,omitempty"`

	// NodeReady mirrors the target-cluster node's Ready condition at last observe.
	// +optional
	NodeReady bool `json:"nodeReady,omitempty"`

	// GPURegistered is whether the node advertised GPU allocatable at last observe.
	// +optional
	GPURegistered bool `json:"gpuRegistered,omitempty"`

	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastTransitionTime is when Phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`

	// Conditions follows the standard metav1.Condition pattern.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types and reasons.
const (
	// ConditionReady is True when the node is in service (powered on + Ready).
	ConditionReady = "Ready"
	// ConditionConverged is True when observed matches spec.desiredPower.
	ConditionConverged = "Converged"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=en
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredPower`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Power",type=string,JSONPath=`.status.observedPower`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.nodeReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ElasticNode is the desired power-state record for one target-cluster node.
type ElasticNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticNodeSpec   `json:"spec,omitempty"`
	Status ElasticNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ElasticNodeList is a list of ElasticNode.
type ElasticNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ElasticNode `json:"items"`
}
