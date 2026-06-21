package main

import (
	"strings"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
)

const wakeInvYAML = `
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
    gpus: ["example-gpu"]
`

func gpuNodeSpec() inventory.NodeSpec {
	return inventory.NodeSpec{
		KubeNodeName:  "node-1",
		TalosEndpoint: "192.0.2.10",
		BMC:           inventory.BMCConfig{Type: "amt", Host: "192.0.2.1"},
		GPUs:          []string{"example-gpu"},
	}
}

func TestWakePlanWaitsForGPU(t *testing.T) {
	c := wakeConfig{reachableTimeout: 5 * time.Minute, readyTimeout: 5 * time.Minute, gpuTimeout: 3 * time.Minute, poll: 5 * time.Second}
	plan := wakePlan("wkr-01", "node-1", gpuNodeSpec(), true, c)
	for _, want := range []string{"node-1", "192.0.2.10", "amt/192.0.2.1", "wait_gpu=yes (example-gpu)", "gpu=3m"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
}

func TestWakePlanNoGPU(t *testing.T) {
	plan := wakePlan("wkr-01", "node-1", gpuNodeSpec(), false, wakeConfig{})
	if !strings.Contains(plan, "wait_gpu=no") {
		t.Errorf("plan should show wait_gpu=no when not expecting a GPU:\n%s", plan)
	}
}

func TestRunWakeUnknownNode(t *testing.T) {
	inv, err := inventory.Load([]byte(wakeInvYAML))
	if err != nil {
		t.Fatal(err)
	}
	if rc := runWake(inv, "nope", wakeConfig{}); rc != 1 {
		t.Fatalf("unknown node rc = %d, want 1", rc)
	}
}

func TestRunWakeDryRun(t *testing.T) {
	inv, err := inventory.Load([]byte(wakeInvYAML))
	if err != nil {
		t.Fatal(err)
	}
	if rc := runWake(inv, "node-1", wakeConfig{dryRun: true}); rc != 0 {
		t.Fatalf("dry-run rc = %d, want 0", rc)
	}
}
