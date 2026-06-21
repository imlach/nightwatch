package controller

import (
	"context"
	"errors"

	"github.com/imlach/nightwatch/internal/inventory"
)

// TargetCluster names a workload cluster and its OOB endpoints. The real provider
// will resolve ElasticNode.spec.clusterRef to one of these.
type TargetCluster struct {
	Name          string
	Kubeconfig    string // path to the target-cluster kubeconfig
	TalosConfig   string // path to the target-cluster talosconfig
	InventoryPath string // git inventory (node identity + BMC/Talos endpoints)
}

// ClusterProvider is the real Backends implementation: it builds the target kube
// client + OOB adapters per node. Stubbed for the skeleton - the sim/integration
// path already proves bmc.New/talos.New/iscsi.Gate work end-to-end, so this seam
// just needs the cross-cluster wiring as the next increment.
type ClusterProvider struct {
	Target    TargetCluster
	Inventory *inventory.Inventory // loaded from InventoryPath / a ConfigMap
}

// errProviderStub marks the cross-cluster wiring as not-yet-implemented; the
// reconciler surfaces it as PhaseError and requeues (no bad actuation).
var errProviderStub = errors.New("ClusterProvider not wired yet")

// BackendsFor is the deferred cross-cluster join.
//
// TODO: build the target-cluster client from Target.Kubeconfig
// (k8s.NewFromKubeconfig), join inventory.NodeSpec by name, construct
// bmc.New(type,host,user,pass) / talos.New(talosconfig, endpoint) /
// iscsi.Gate{List: ...} bound to NodeSpec.ISCSIIQN, and assemble the
// lifecycle.DrainShutdownDeps / PowerOnDeps + per-node options (Talos endpoint,
// ExpectGPU from len(GPUs)>0, timeouts). The sim/integration tests already
// exercise those adapters; this is wiring, not new behaviour.
func (p *ClusterProvider) BackendsFor(ctx context.Context, node string) (*NodeBackends, error) {
	return nil, errProviderStub
}

// Compile-time assertion: the stub satisfies the seam.
var _ Backends = (*ClusterProvider)(nil)
