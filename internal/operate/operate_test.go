package operate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
)

const invYAML = `
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    iscsi_initiator_addr: "198.51.100.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
  node-2:
    talos_endpoint: "192.0.2.11"
    iscsi_initiator_addr: "198.51.100.11"
    kube_node_name: node-2
    bmc: {type: amt, host: "192.0.2.2"}
    gpus: ["example-gpu"]
`

func loadInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Load([]byte(invYAML))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// fakeNodes satisfies NodeOps (both drain + wake k8s surfaces). It tracks the
// cordon/uncordon transitions and reports a Ready, GPU-bearing node.
type fakeNodes struct {
	cordoned, uncordoned bool
}

func (f *fakeNodes) Cordon(context.Context, string) error                  { f.cordoned = true; return nil }
func (f *fakeNodes) Drain(context.Context, string, k8s.DrainOptions) error { return nil }
func (f *fakeNodes) Uncordon(context.Context, string) error                { f.uncordoned = true; return nil }
func (f *fakeNodes) IsNodeReady(context.Context, string) (bool, error)     { return true, nil }
func (f *fakeNodes) NodeHasGPUCapacity(context.Context, string) (bool, error) {
	return true, nil
}
func (f *fakeNodes) IsNodeSchedulable(context.Context, string) (bool, error) { return !f.cordoned, nil }

// fakePower is a power adapter (bmc.Adapter) whose state follows talos shutdown.
type fakePower struct{ on bool }

func (f *fakePower) GetPowerState(context.Context) bmc.Result {
	st := bmc.PowerOff
	if f.on {
		st = bmc.PowerOn
	}
	return bmc.Result{OK: true, PowerState: st}
}
func (f *fakePower) PowerOn(context.Context) bmc.Result {
	f.on = true
	return bmc.Result{OK: true, PowerState: bmc.PowerOn}
}
func (f *fakePower) SoftOff(context.Context) bmc.Result { return bmc.Result{OK: true} }
func (f *fakePower) HardOff(context.Context) bmc.Result {
	f.on = false
	return bmc.Result{OK: true, PowerState: bmc.PowerOff}
}
func (f *fakePower) Reset(context.Context) bmc.Result { return bmc.Result{OK: true} }

// fakeTalos models the OS shutdown powering the node off (drain side) and an
// always-reachable API (wake side).
type fakeTalos struct{ power *fakePower }

func (f *fakeTalos) Shutdown(context.Context, string) error { f.power.on = false; return nil }
func (f *fakeTalos) Reachable(context.Context, string) bool { return true }

// fakeBuilder wires the fakes as a Builder, sharing one power state so the talos
// shutdown drives the BMC power-off the wait loop observes.
func fakeBuilder(nodes *fakeNodes, power *fakePower) Builder {
	return func(context.Context, inventory.NodeSpec, Config) (Backends, error) {
		return Backends{
			Nodes:   nodes,
			Talos:   &fakeTalos{power: power},
			Power:   power,
			Storage: lifecycle.StorageGateFunc(func(context.Context, time.Duration) error { return nil }),
		}, nil
	}
}

func fastCfg() Config {
	return Config{Poll: time.Millisecond, DrainTimeout: time.Second, StorageTimeout: time.Second,
		PowerOffTimeout: time.Second, ReachableTimeout: time.Second, ReadyTimeout: time.Second, GPUTimeout: time.Second}
}

func TestDrainShutdownConverges(t *testing.T) {
	inv := loadInv(t)
	nodes := &fakeNodes{}
	power := &fakePower{on: true}
	steps, err := DrainShutdown(context.Background(), inv, "node-1", fastCfg(), fakeBuilder(nodes, power))
	if err != nil {
		t.Fatalf("DrainShutdown = %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("no steps recorded")
	}
	for _, s := range steps {
		if !s.Succeeded {
			t.Errorf("step %q failed: %s", s.Name, s.Message)
		}
	}
	if last := steps[len(steps)-1]; last.Name != "wait-power-off" {
		t.Errorf("final step = %q, want wait-power-off", last.Name)
	}
	if !nodes.cordoned {
		t.Error("node not cordoned")
	}
	if power.on {
		t.Error("node still on after drain-shutdown")
	}
}

func TestWakeConverges(t *testing.T) {
	inv := loadInv(t)
	nodes := &fakeNodes{}
	power := &fakePower{on: false}
	steps, err := Wake(context.Background(), inv, "node-2", fastCfg(), fakeBuilder(nodes, power))
	if err != nil {
		t.Fatalf("Wake = %v", err)
	}
	for _, s := range steps {
		if !s.Succeeded {
			t.Errorf("step %q failed: %s", s.Name, s.Message)
		}
	}
	if last := steps[len(steps)-1]; last.Name != "uncordon" {
		t.Errorf("final step = %q, want uncordon", last.Name)
	}
	if !nodes.uncordoned {
		t.Error("node not uncordoned")
	}
	if !power.on {
		t.Error("node not powered on after wake")
	}
	// GPU-bearing node must gate on GPU registration.
	var sawGPU bool
	for _, s := range steps {
		if s.Name == "wait-gpu-registered" && s.Succeeded && s.Details["skipped"] == nil {
			sawGPU = true
		}
	}
	if !sawGPU {
		t.Error("expected a non-skipped wait-gpu-registered step for a GPU node")
	}
}

func TestWakeSkipGPU(t *testing.T) {
	inv := loadInv(t)
	steps, err := Wake(context.Background(), inv, "node-2", func() Config { c := fastCfg(); c.SkipGPUWait = true; return c }(), fakeBuilder(&fakeNodes{}, &fakePower{}))
	if err != nil {
		t.Fatalf("Wake = %v", err)
	}
	for _, s := range steps {
		if s.Name == "wait-gpu-registered" && s.Details["skipped"] == nil {
			t.Error("SkipGPUWait should mark wait-gpu-registered skipped")
		}
	}
}

func TestUnknownNode(t *testing.T) {
	inv := loadInv(t)
	if _, err := DrainShutdown(context.Background(), inv, "nope", fastCfg(), fakeBuilder(&fakeNodes{}, &fakePower{})); err == nil {
		t.Fatal("want error for unknown node")
	} else {
		var unknown *UnknownNodeError
		if !errors.As(err, &unknown) {
			t.Errorf("err = %v, want *UnknownNodeError", err)
		}
	}
	if _, err := Wake(context.Background(), inv, "nope", fastCfg(), fakeBuilder(&fakeNodes{}, &fakePower{})); err == nil {
		t.Fatal("want error for unknown node on wake")
	}
}

func TestStorageGateIdentityUsesExplicitInventoryField(t *testing.T) {
	node := inventory.NodeSpec{
		TalosEndpoint:      "192.0.2.10",
		ISCSIInitiatorAddr: "198.51.100.10",
	}
	if got := StorageGateIdentity(node); got != "198.51.100.10" {
		t.Fatalf("StorageGateIdentity() = %q, want explicit iscsi_initiator_addr", got)
	}
}

func TestStorageGateIdentityDoesNotFallbackToTalosEndpoint(t *testing.T) {
	node := inventory.NodeSpec{TalosEndpoint: "192.0.2.10"}
	if got := StorageGateIdentity(node); got != "" {
		t.Fatalf("StorageGateIdentity() = %q, want empty without explicit storage identity", got)
	}
}

func TestBuildStorageGateRequiresIdentityWhenTrueNASConfigured(t *testing.T) {
	t.Setenv("NIGHTWATCH_TRUENAS_HOST", "storage.example.com")
	t.Setenv("NIGHTWATCH_TRUENAS_USERNAME", "nightwatch")
	t.Setenv("NIGHTWATCH_TRUENAS_API_KEY", "3-secret")
	sg, closeFn, err := BuildStorageGate(context.Background(), "")
	if err == nil {
		t.Fatal("BuildStorageGate() error = nil, want missing identity error")
	}
	if sg != nil || closeFn != nil {
		t.Fatal("BuildStorageGate() should not return a gate or closer when identity is missing")
	}
	if got := err.Error(); !strings.Contains(got, "iscsi_initiator_addr") {
		t.Fatalf("BuildStorageGate() error = %q, want iscsi_initiator_addr", got)
	}
}

func TestLazyStorageGateDefersIdentityErrorUntilWait(t *testing.T) {
	t.Setenv("NIGHTWATCH_TRUENAS_HOST", "storage.example.com")
	t.Setenv("NIGHTWATCH_TRUENAS_USERNAME", "nightwatch")
	t.Setenv("NIGHTWATCH_TRUENAS_API_KEY", "3-secret")

	gate := lazyStorageGate("")
	if gate == nil {
		t.Fatal("lazyStorageGate() = nil, want configured gate")
	}
	if err := gate.WaitDetached(context.Background(), time.Second); err == nil {
		t.Fatal("WaitDetached() error = nil, want missing identity error")
	} else if got := err.Error(); !strings.Contains(got, "iscsi_initiator_addr") {
		t.Fatalf("WaitDetached() error = %q, want iscsi_initiator_addr", got)
	}
}
