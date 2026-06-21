package controller

import (
	"context"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/lifecycle"
)

// Backends builds the per-node lifecycle backends for one ElasticNode. It is the
// SEAM that keeps the cross-cluster plumbing out of the reconciler: the provider
// joins the CR (by node name / clusterRef) to the target-cluster kube client and
// the OOB BMC/Talos/TrueNAS adapters, and returns ready-to-drive deps.
//
// The reconciler depends only on this interface, so a real provider (target
// kubeconfig + inventory ConfigMap + bmc.New/talos.New/iscsi.Gate) and an
// in-process fake are swapped at wiring time without touching reconcile logic.
type Backends interface {
	// BackendsFor returns the lifecycle backends for the named node. node is the
	// ElasticNode object name (which equals the kube node name unless the
	// inventory remaps it). Errors are transient - the reconciler requeues.
	BackendsFor(ctx context.Context, node string) (*NodeBackends, error)
}

// NodeBackends bundles everything the reconciler needs to drive one node both
// directions, plus the live-observe handles. The lifecycle.*Deps reuse the
// existing convergent state machines verbatim - the reconciler picks direction
// and calls them, it does NOT reimplement steps.
type NodeBackends struct {
	// Power reads the node's BMC power state for the desired-vs-observed decision
	// (independent of the target kube API, so it works during an outage).
	Power lifecycle.PowerController

	// Drain and PowerOn are the convergent state-machine inputs.
	Drain   lifecycle.DrainShutdownDeps
	PowerOn lifecycle.PowerOnDeps

	// Ready / GPU observe the target-cluster node (best-effort; may error while
	// the node is down, which the reconciler treats as not-ready, not fatal).
	Gater lifecycle.NodeGater

	// Timeouts/options resolved per node (Talos endpoint, GPU expectation, etc.).
	DrainOpts   lifecycle.DrainShutdownOptions
	PowerOnOpts lifecycle.PowerOnOptions
}

// observePower reads the BMC power state, mapping it to the API enum. An unknown
// or errored read returns ("", false) - the reconciler must not act on a guess.
func observePower(ctx context.Context, p lifecycle.PowerController) (bmc.PowerState, bool) {
	res := p.GetPowerState(ctx)
	if !res.OK {
		return bmc.PowerUnknown, false
	}
	return res.PowerState, true
}

// defaultResync is the steady level-triggered re-derive cadence: even with no CR
// change, re-observe the world and converge (a node that crashed off while
// desired=On gets re-driven up).
const defaultResync = 60 * time.Second
