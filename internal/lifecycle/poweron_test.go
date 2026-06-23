package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/operation"
)

type fakeStarter struct {
	state        bmc.PowerState
	stateOK      bool
	powerOnOK    bool
	powerOnCalls int
}

func (f *fakeStarter) GetPowerState(context.Context) bmc.Result {
	return bmc.Result{OK: f.stateOK, PowerState: f.state}
}

func (f *fakeStarter) PowerOn(context.Context) bmc.Result {
	f.powerOnCalls++
	if !f.powerOnOK {
		return bmc.Result{Error: "amt refused"}
	}
	return bmc.Result{OK: true, PowerState: bmc.PowerOn}
}

type fakeReach struct {
	readyAfter int
	calls      int
}

func (f *fakeReach) Reachable(context.Context, string) bool {
	f.calls++
	return f.calls >= f.readyAfter
}

type fakeGater struct {
	readyAfter, gpuAfter int
	readyCalls, gpuCalls int
	readyErr             error
	uncordoned           bool
	uncordonErr          error
}

func (f *fakeGater) IsNodeReady(context.Context, string) (bool, error) {
	f.readyCalls++
	if f.readyErr != nil {
		return false, f.readyErr
	}
	return f.readyCalls >= f.readyAfter, nil
}

func (f *fakeGater) NodeHasGPUCapacity(context.Context, string) (bool, error) {
	f.gpuCalls++
	return f.gpuCalls >= f.gpuAfter, nil
}

func (f *fakeGater) IsNodeSchedulable(context.Context, string) (bool, error) {
	return true, nil
}

func (f *fakeGater) Uncordon(context.Context, string) error {
	f.uncordoned = true
	return f.uncordonErr
}

func runPowerOn(p *fakeStarter, r *fakeReach, g *fakeGater, expectGPU bool) ([]operation.Step, error) {
	return PowerOn(context.Background(), "node-1", PowerOnDeps{Power: p, Talos: r, Nodes: g}, PowerOnOptions{
		TalosEndpoint:    "192.0.2.10",
		ExpectGPU:        expectGPU,
		ReachableTimeout: 50 * time.Millisecond,
		ReadyTimeout:     50 * time.Millisecond,
		GPUTimeout:       50 * time.Millisecond,
		PollInterval:     time.Millisecond,
	})
}

func stepNames(steps []operation.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func TestPowerOnHappyPath(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: true}
	g := &fakeGater{readyAfter: 1, gpuAfter: 1}
	steps, err := runPowerOn(p, &fakeReach{readyAfter: 1}, g, true)
	if err != nil {
		t.Fatalf("PowerOn = %v, want nil", err)
	}
	if p.powerOnCalls != 1 {
		t.Errorf("PowerOn calls = %d, want 1", p.powerOnCalls)
	}
	if !g.uncordoned {
		t.Error("node was not uncordoned on success")
	}
	want := []string{"bmc-power-on", "wait-talos-reachable", "wait-node-ready", "wait-gpu-registered", "uncordon"}
	if got := stepNames(steps); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

func TestPowerOnAlreadyOnSkipsBMC(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOn, stateOK: true}
	g := &fakeGater{readyAfter: 1, gpuAfter: 1}
	if _, err := runPowerOn(p, &fakeReach{readyAfter: 1}, g, false); err != nil {
		t.Fatalf("PowerOn = %v, want nil", err)
	}
	if p.powerOnCalls != 0 {
		t.Errorf("PowerOn called %d times though node was already on", p.powerOnCalls)
	}
}

func TestPowerOnNilTalosSkipsReachability(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: true}
	g := &fakeGater{readyAfter: 1}
	steps, err := PowerOn(context.Background(), "node-1", PowerOnDeps{Power: p, Nodes: g}, PowerOnOptions{
		TalosEndpoint:    "192.0.2.10",
		ReachableTimeout: 50 * time.Millisecond,
		ReadyTimeout:     50 * time.Millisecond,
		PollInterval:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("PowerOn = %v, want nil", err)
	}
	if !g.uncordoned {
		t.Error("node should still be uncordoned after readiness passes")
	}
	want := []string{"bmc-power-on", "wait-talos-reachable", "wait-node-ready", "wait-gpu-registered", "uncordon"}
	if got := stepNames(steps); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("steps = %v, want %v", got, want)
	}
	if got := steps[1].Details["skipped"]; got != "talos reachability unavailable" {
		t.Errorf("talos step skipped detail = %v, want talos reachability unavailable", got)
	}
}

func TestPowerOnBMCFailsStopsBeforeUncordon(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: false}
	r := &fakeReach{readyAfter: 1}
	g := &fakeGater{readyAfter: 1, gpuAfter: 1}
	_, err := runPowerOn(p, r, g, true)
	if err == nil {
		t.Fatal("PowerOn = nil, want error when BMC power-on fails")
	}
	if r.calls != 0 {
		t.Errorf("talos reachability checked %d times after a failed power-on, want 0", r.calls)
	}
	if g.uncordoned {
		t.Error("node uncordoned after a failed power-on")
	}
}

func TestPowerOnSkipsGPUWaitWhenNotExpected(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: true}
	g := &fakeGater{readyAfter: 1}
	if _, err := runPowerOn(p, &fakeReach{readyAfter: 1}, g, false); err != nil {
		t.Fatalf("PowerOn = %v, want nil", err)
	}
	if g.gpuCalls != 0 {
		t.Errorf("GPU capacity checked %d times though ExpectGPU=false", g.gpuCalls)
	}
	if !g.uncordoned {
		t.Error("non-GPU node should still be uncordoned")
	}
}

func TestPowerOnGPUTimeoutStaysCordoned(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: true}
	g := &fakeGater{readyAfter: 1, gpuAfter: 1 << 30} // GPU never registers in the window
	_, err := runPowerOn(p, &fakeReach{readyAfter: 1}, g, true)
	if err == nil || !strings.Contains(err.Error(), "gpu registered") {
		t.Fatalf("PowerOn = %v, want gpu-registration timeout", err)
	}
	if g.uncordoned {
		t.Error("node must stay cordoned when the GPU never registers")
	}
}

func TestPowerOnReadyErrorAborts(t *testing.T) {
	p := &fakeStarter{state: bmc.PowerOff, stateOK: true, powerOnOK: true}
	g := &fakeGater{readyErr: errors.New("apiserver down")}
	_, err := runPowerOn(p, &fakeReach{readyAfter: 1}, g, false)
	if err == nil {
		t.Fatal("PowerOn = nil, want error when readiness check errors")
	}
	if g.uncordoned {
		t.Error("node must stay cordoned on a readiness API error")
	}
}
