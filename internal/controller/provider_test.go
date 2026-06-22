package controller

import (
	"context"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/operate"
)

// provFake satisfies operate.NodeOps + Talos shutdown/reachable + bmc.Adapter.
// BackendsFor only WIRES these (never calls them), so the bodies are trivial.
type provFake struct{}

func (provFake) Cordon(context.Context, string) error                     { return nil }
func (provFake) Drain(context.Context, string, k8s.DrainOptions) error    { return nil }
func (provFake) IsNodeReady(context.Context, string) (bool, error)        { return true, nil }
func (provFake) NodeHasGPUCapacity(context.Context, string) (bool, error) { return true, nil }
func (provFake) Uncordon(context.Context, string) error                   { return nil }
func (provFake) Shutdown(context.Context, string) error                   { return nil }
func (provFake) Reachable(context.Context, string) bool                   { return true }
func (provFake) GetPowerState(context.Context) bmc.Result                 { return bmc.Result{} }
func (provFake) PowerOn(context.Context) bmc.Result                       { return bmc.Result{} }
func (provFake) SoftOff(context.Context) bmc.Result                       { return bmc.Result{} }
func (provFake) HardOff(context.Context) bmc.Result                       { return bmc.Result{} }
func (provFake) Reset(context.Context) bmc.Result                         { return bmc.Result{} }

func provInv(elastic bool, gpus []string) *inventory.Inventory {
	return &inventory.Inventory{Nodes: map[string]inventory.NodeSpec{
		"wkr-04": {
			ElasticEligible: elastic,
			TalosEndpoint:   "10.0.0.94",
			GPUs:            gpus,
			BMC:             inventory.BMCConfig{Type: "amt", Host: "10.0.10.94"},
		},
	}}
}

func TestBackendsForMapsAndCloses(t *testing.T) {
	var fb provFake
	closed := false
	p := &ClusterProvider{
		Inventory: provInv(true, []string{"3090"}),
		Config:    operate.Config{DrainTimeout: time.Minute, Poll: time.Second},
		Build: func(context.Context, inventory.NodeSpec, operate.Config) (operate.Backends, error) {
			return operate.Backends{
				Nodes:   fb,
				Talos:   fb,
				Power:   fb,
				Storage: lifecycle.StorageGateFunc(func(context.Context, time.Duration) error { return nil }),
				Close:   func() { closed = true },
			}, nil
		},
	}

	be, err := p.BackendsFor(context.Background(), BackendRef{Node: "wkr-04"})
	if err != nil {
		t.Fatalf("BackendsFor: %v", err)
	}
	if be.DrainOpts.TalosEndpoint != "10.0.0.94" {
		t.Errorf("DrainOpts.TalosEndpoint = %q, want 10.0.0.94", be.DrainOpts.TalosEndpoint)
	}
	if be.DrainOpts.DrainTimeout != time.Minute {
		t.Errorf("DrainTimeout not propagated from Config: %v", be.DrainOpts.DrainTimeout)
	}
	if !be.PowerOnOpts.ExpectGPU {
		t.Error("ExpectGPU should be true for a GPU-bearing node")
	}
	if be.PowerOn.Talos == nil {
		t.Error("PowerOn.Talos should be set (the builder's Talos is Reachable)")
	}
	if be.Power == nil || be.Gater == nil || be.Drain.Storage == nil {
		t.Error("Power/Gater/Storage not wired")
	}
	if be.Close == nil {
		t.Fatal("Close not wired")
	}
	be.Close()
	if !closed {
		t.Error("Close did not run the builder's closer")
	}
}

func TestBackendsForGuards(t *testing.T) {
	cases := []struct {
		name string
		p    *ClusterProvider
		ref  BackendRef
	}{
		{"nil inventory", &ClusterProvider{}, BackendRef{Node: "wkr-04"}},
		{"unknown node", &ClusterProvider{Inventory: provInv(true, nil)}, BackendRef{Node: "nope"}},
		{"not elastic", &ClusterProvider{Inventory: provInv(false, nil)}, BackendRef{Node: "wkr-04"}},
		{"wrong cluster", &ClusterProvider{Target: TargetCluster{Name: "default"}, Inventory: provInv(true, nil)}, BackendRef{Node: "wkr-04", Cluster: "other"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.p.BackendsFor(context.Background(), c.ref); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}
