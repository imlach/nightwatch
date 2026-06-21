package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/bmc/amtwsman"
	"github.com/imlach/nightwatch/internal/iscsi"
	"github.com/imlach/nightwatch/internal/k8s"
)

// The real backends must satisfy the swappable orchestrator interfaces.
var (
	_ NodeController  = (*k8s.Client)(nil)
	_ PowerController = (*amtwsman.Client)(nil) // any bmc.Adapter (AMT/Redfish/IPMI) fits
	_ StorageGate     = StorageGateFunc(nil)
)

type fakeNodes struct {
	cordoned, drained   bool
	cordonErr, drainErr error
}

func (f *fakeNodes) Cordon(context.Context, string) error { f.cordoned = true; return f.cordonErr }
func (f *fakeNodes) Drain(context.Context, string, k8s.DrainOptions) error {
	f.drained = true
	return f.drainErr
}

type fakeTalos struct {
	called     bool
	err        error
	onShutdown func() // simulate the OS shutdown powering the node off
}

func (f *fakeTalos) Shutdown(context.Context, string) error {
	f.called = true
	if f.err == nil && f.onShutdown != nil {
		f.onShutdown()
	}
	return f.err
}

type fakePower struct {
	on              bool
	hardOffOK       bool
	offAfterHardOff bool
	hardOffCalls    int
}

func (f *fakePower) GetPowerState(context.Context) bmc.Result {
	st := bmc.PowerOff
	if f.on {
		st = bmc.PowerOn
	}
	return bmc.Result{OK: true, PowerState: st}
}

func (f *fakePower) HardOff(context.Context) bmc.Result {
	f.hardOffCalls++
	if !f.hardOffOK {
		return bmc.Result{OK: false, Error: "hard-off failed"}
	}
	if f.offAfterHardOff {
		f.on = false
	}
	return bmc.Result{OK: true, PowerState: bmc.PowerOff}
}

type fakeGate struct {
	called bool
	err    error
}

func (f *fakeGate) WaitDetached(context.Context, time.Duration) error { f.called = true; return f.err }

func fastOpts(force bool) DrainShutdownOptions {
	return DrainShutdownOptions{
		TalosEndpoint:   "192.0.2.10",
		PollInterval:    time.Millisecond,
		PowerOffTimeout: 30 * time.Millisecond,
		StorageTimeout:  30 * time.Millisecond,
		ForceBMCOff:     force,
	}
}

func run(t *testing.T, deps DrainShutdownDeps, opts DrainShutdownOptions) ([]string, map[string]bool, error) {
	t.Helper()
	steps, err := DrainShutdown(context.Background(), "node-1", deps, opts)
	var ns []string
	ok := map[string]bool{}
	for _, s := range steps {
		ns = append(ns, s.Name)
		ok[s.Name] = s.Succeeded
	}
	return ns, ok, err
}

func TestDrainShutdownHappyPath(t *testing.T) {
	power := &fakePower{on: true}
	talos := &fakeTalos{onShutdown: func() { power.on = false }} // graceful shutdown powers it off
	gate := &fakeGate{}
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: talos, Power: power, Storage: gate}

	ns, ok, err := run(t, deps, fastOpts(false))
	if err != nil {
		t.Fatalf("DrainShutdown = %v, want nil", err)
	}
	want := []string{"cordon", "drain", "storage-gate", "talos-shutdown", "wait-power-off"}
	if len(ns) != len(want) {
		t.Fatalf("steps = %v, want %v", ns, want)
	}
	for _, n := range want {
		if !ok[n] {
			t.Errorf("step %q not succeeded", n)
		}
	}
	if !gate.called {
		t.Error("storage gate not invoked")
	}
	if !talos.called {
		t.Error("talos shutdown not called")
	}
}

func TestDrainShutdownSkipsStorageGateWhenNil(t *testing.T) {
	power := &fakePower{on: true}
	talos := &fakeTalos{onShutdown: func() { power.on = false }}
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: talos, Power: power, Storage: nil}

	_, ok, err := run(t, deps, fastOpts(false))
	if err != nil {
		t.Fatal(err)
	}
	if !ok["storage-gate"] {
		t.Error("skipped storage-gate step should still be recorded as succeeded")
	}
}

func TestDrainShutdownStopsOnDrainFailure(t *testing.T) {
	talos := &fakeTalos{}
	deps := DrainShutdownDeps{Nodes: &fakeNodes{drainErr: errors.New("pdb blocked")}, Talos: talos, Power: &fakePower{}, Storage: &fakeGate{}}

	ns, _, err := run(t, deps, fastOpts(false))
	if err == nil {
		t.Fatal("want error on drain failure")
	}
	if talos.called {
		t.Error("talos shutdown must not run after a failed drain")
	}
	if ns[len(ns)-1] != "drain" {
		t.Errorf("last step = %q, want drain", ns[len(ns)-1])
	}
}

func TestDrainShutdownStorageGateBlocks(t *testing.T) {
	talos := &fakeTalos{}
	gate := &fakeGate{err: errors.New("volumes still attached")}
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: talos, Power: &fakePower{}, Storage: gate}

	_, ok, err := run(t, deps, fastOpts(false))
	if err == nil {
		t.Fatal("want error when storage doesn't detach")
	}
	if talos.called {
		t.Error("must not shut down with storage still attached")
	}
	if ok["storage-gate"] {
		t.Error("storage-gate step should be marked failed")
	}
}

func TestDrainShutdownPowerStuckNoForce(t *testing.T) {
	power := &fakePower{on: true} // graceful shutdown is a no-op below → never powers off
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: &fakeTalos{}, Power: power, Storage: &fakeGate{}}

	_, _, err := run(t, deps, fastOpts(false))
	if err == nil {
		t.Fatal("want error when node never powers off and ForceBMCOff is false")
	}
	if power.hardOffCalls != 0 {
		t.Error("hard-off must not fire without ForceBMCOff")
	}
}

func TestDrainShutdownForceHardOff(t *testing.T) {
	power := &fakePower{on: true, hardOffOK: true, offAfterHardOff: true}
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: &fakeTalos{}, Power: power, Storage: &fakeGate{}}

	ns, ok, err := run(t, deps, fastOpts(true))
	if err != nil {
		t.Fatalf("DrainShutdown = %v, want nil after successful hard-off", err)
	}
	if power.hardOffCalls != 1 {
		t.Errorf("hard-off calls = %d, want 1", power.hardOffCalls)
	}
	if !ok["bmc-hard-off"] || !ok["wait-power-off-after-hardoff"] {
		t.Errorf("hard-off steps not recorded ok: %v", ns)
	}
}

func TestDrainShutdownForceHardOffFails(t *testing.T) {
	power := &fakePower{on: true, hardOffOK: false} // hard-off rejected by the BMC
	deps := DrainShutdownDeps{Nodes: &fakeNodes{}, Talos: &fakeTalos{}, Power: power, Storage: &fakeGate{}}

	ns, ok, err := run(t, deps, fastOpts(true))
	if err == nil {
		t.Fatal("want error when hard-off fails")
	}
	if ok["bmc-hard-off"] {
		t.Error("bmc-hard-off step should be marked failed")
	}
	var hardOffSteps int
	for _, n := range ns {
		if n == "bmc-hard-off" {
			hardOffSteps++
		}
	}
	if hardOffSteps != 1 {
		t.Errorf("bmc-hard-off recorded %d times, want 1: %v", hardOffSteps, ns)
	}
}

// TestStorageGateBindsISCSI shows the modular storage backend wiring: the
// node-agnostic iscsi.Gate is bound to a node's initiator IQN as a StorageGate.
func TestStorageGateBindsISCSI(t *testing.T) {
	g := iscsi.Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) { return "no sessions", nil }}
	var gate StorageGate = StorageGateFunc(func(ctx context.Context, to time.Duration) error {
		return g.WaitClear(ctx, "iqn.2005-03.org.open-iscsi:node-1", to)
	})
	if err := gate.WaitDetached(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatalf("bound iscsi gate WaitDetached = %v", err)
	}
}
