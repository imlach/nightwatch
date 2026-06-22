package controller

import (
	"context"
	"fmt"

	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/operate"
)

// TargetCluster names a workload cluster and its OOB endpoints. The provider
// resolves an ElasticNode to one of these.
type TargetCluster struct {
	Name          string
	Kubeconfig    string // path to the target-cluster kubeconfig
	TalosConfig   string // path to the target-cluster talosconfig
	InventoryPath string // git inventory (node identity + BMC/Talos endpoints)
}

// ClusterProvider is the real Backends implementation. Per node it builds the
// target-cluster kube client + OOB adapters by reusing the proven serve/CLI
// assembly (operate.RealBuilder), pointed at the target kubeconfig/talosconfig.
type ClusterProvider struct {
	Target    TargetCluster
	Inventory *inventory.Inventory // node identity, loaded from InventoryPath / a ConfigMap

	// BMCCreds resolves BMC username/password by bmc.type ("amt", "idrac"/"redfish").
	// Injected by main from the operator's mounted Secret; the inventory carries no
	// secrets (NodeSpec.BMC.Username/Password are yaml:"-"). nil -> empty creds.
	BMCCreds func(bmcType string) (user, pass string)

	// Config carries the lifecycle timeouts/poll (drain/storage/poweroff/wake);
	// Kubeconfig/Talosconfig are overridden per call from Target.
	Config operate.Config

	// Build wires the live backends for a node; defaults to operate.RealBuilder.
	// Tests inject a fake (the sims for bmc/truenas, fakes for k8s/talos).
	Build operate.Builder
}

// BackendsFor joins the ElasticNode (by node name) to the target-cluster client
// and the OOB BMC/Talos/TrueNAS adapters, returning ready-to-drive deps. Errors
// are transient - the reconciler surfaces PhaseError and requeues.
func (p *ClusterProvider) BackendsFor(ctx context.Context, ref BackendRef) (*NodeBackends, error) {
	if ref.Cluster != "" && ref.Cluster != p.Target.Name {
		return nil, fmt.Errorf("node %q requests clusterRef %q, provider target is %q", ref.Node, ref.Cluster, p.Target.Name)
	}
	if p.Inventory == nil {
		return nil, fmt.Errorf("inventory not loaded")
	}
	spec, ok := p.Inventory.Nodes[ref.Node]
	if !ok {
		return nil, fmt.Errorf("node %q not in inventory", ref.Node)
	}
	if !spec.ElasticEligible {
		return nil, fmt.Errorf("node %q is not elastic_eligible", ref.Node)
	}
	if p.BMCCreds != nil {
		spec.BMC.Username, spec.BMC.Password = p.BMCCreds(spec.BMC.Type)
	}

	cfg := p.Config
	cfg.Kubeconfig = p.Target.Kubeconfig
	cfg.Talosconfig = p.Target.TalosConfig

	build := p.Build
	if build == nil {
		build = operate.RealBuilder
	}
	be, err := build(ctx, spec, cfg)
	if err != nil {
		return nil, err
	}

	// PowerOn needs a reachability probe; the Talos adapter satisfies both
	// shutdown and reachable. A builder that returns a shutdown-only Talos leaves
	// PowerOn.Talos nil, which the state machine treats as "skip the probe".
	powerOn := lifecycle.PowerOnDeps{Power: be.Power, Nodes: be.Nodes}
	if r, ok := be.Talos.(lifecycle.TalosReachable); ok {
		powerOn.Talos = r
	}

	return &NodeBackends{
		Power: be.Power,
		Drain: lifecycle.DrainShutdownDeps{
			Nodes: be.Nodes, Talos: be.Talos, Power: be.Power, Storage: be.Storage,
		},
		PowerOn: powerOn,
		Gater:   be.Nodes,
		DrainOpts: lifecycle.DrainShutdownOptions{
			TalosEndpoint:   spec.TalosEndpoint,
			DrainTimeout:    cfg.DrainTimeout,
			StorageTimeout:  cfg.StorageTimeout,
			PowerOffTimeout: cfg.PowerOffTimeout,
			PollInterval:    cfg.Poll,
			ForceBMCOff:     cfg.ForceBMCOff,
		},
		PowerOnOpts: lifecycle.PowerOnOptions{
			TalosEndpoint:    spec.TalosEndpoint,
			ExpectGPU:        len(spec.GPUs) > 0,
			ReachableTimeout: cfg.ReachableTimeout,
			ReadyTimeout:     cfg.ReadyTimeout,
			GPUTimeout:       cfg.GPUTimeout,
			PollInterval:     cfg.Poll,
		},
		Close: be.Close,
	}, nil
}

// Compile-time assertion: the provider satisfies the seam.
var _ Backends = (*ClusterProvider)(nil)
