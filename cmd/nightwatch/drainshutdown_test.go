package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
)

const testInvYAML = `
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
`

func testNode() inventory.NodeSpec {
	return inventory.NodeSpec{
		KubeNodeName:  "node-1",
		TalosEndpoint: "192.0.2.10",
		BMC:           inventory.BMCConfig{Type: "amt", Host: "192.0.2.1"},
	}
}

func clearTrueNASEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NIGHTWATCH_TRUENAS_HOST", "")
	t.Setenv("NIGHTWATCH_TRUENAS_USERNAME", "")
	t.Setenv("NIGHTWATCH_TRUENAS_API_KEY", "")
}

func TestDrainShutdownPlanSkipsStorageWithoutCreds(t *testing.T) {
	clearTrueNASEnv(t)
	c := drainShutdownConfig{drainTimeout: 5 * time.Minute, storageTimeout: 2 * time.Minute, powerOffTimeout: 5 * time.Minute, poll: 5 * time.Second}
	plan := drainShutdownPlan("wkr-01", "node-1", testNode(), c)
	for _, want := range []string{"node-1", "192.0.2.10", "amt/192.0.2.1", "skipped", "drain=5m"} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
}

func TestDrainShutdownPlanShowsGateAndHidesKey(t *testing.T) {
	t.Setenv("NIGHTWATCH_TRUENAS_HOST", "storage.example.com")
	t.Setenv("NIGHTWATCH_TRUENAS_USERNAME", "nightwatch")
	t.Setenv("NIGHTWATCH_TRUENAS_API_KEY", "3-supersecret")
	plan := drainShutdownPlan("wkr-01", "node-1", testNode(), drainShutdownConfig{})
	if !strings.Contains(plan, "initiator_addr=192.0.2.10") {
		t.Errorf("plan should match the gate on the node IP:\n%s", plan)
	}
	if strings.Contains(plan, "3-supersecret") {
		t.Errorf("plan must not leak the api key:\n%s", plan)
	}
}

func TestBuildStorageGateNoCreds(t *testing.T) {
	clearTrueNASEnv(t)
	sg, closeFn, err := buildStorageGate(context.Background(), "192.0.2.10")
	if err != nil {
		t.Fatalf("buildStorageGate without creds err = %v, want nil", err)
	}
	if sg != nil || closeFn != nil {
		t.Fatal("buildStorageGate without creds should return a nil gate and nil closer")
	}
}

func TestRunDrainShutdownUnknownNode(t *testing.T) {
	inv, err := inventory.Load([]byte(testInvYAML))
	if err != nil {
		t.Fatal(err)
	}
	if rc := runDrainShutdown(inv, "nope", drainShutdownConfig{}); rc != 1 {
		t.Fatalf("unknown node rc = %d, want 1", rc)
	}
}

func TestRunDrainShutdownDryRun(t *testing.T) {
	clearTrueNASEnv(t)
	inv, err := inventory.Load([]byte(testInvYAML))
	if err != nil {
		t.Fatal(err)
	}
	if rc := runDrainShutdown(inv, "node-1", drainShutdownConfig{dryRun: true}); rc != 0 {
		t.Fatalf("dry-run rc = %d, want 0", rc)
	}
}
