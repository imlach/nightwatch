package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/imlach/nightwatch/internal/bmc"
)

func TestOperatorRegistersBMCDrivers(t *testing.T) {
	for _, typ := range []string{"amt", "redfish", "idrac"} {
		t.Run(typ, func(t *testing.T) {
			adapter, err := bmc.New(typ, "192.0.2.1", "user", "pass")
			if err != nil {
				t.Fatalf("bmc.New(%q) error = %v, want nil", typ, err)
			}
			if adapter == nil {
				t.Fatalf("bmc.New(%q) adapter = nil, want non-nil", typ)
			}
		})
	}
}

func TestRequiredInventoryFailsForMissingOrInvalidInventory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yml")
	if inv, err := requiredInventory(missing); err == nil {
		t.Fatalf("requiredInventory(missing) = (%v, nil), want error", inv)
	}

	invalid := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalid, []byte("nodes: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if inv, err := requiredInventory(invalid); err == nil {
		t.Fatalf("requiredInventory(invalid) = (%v, nil), want error", inv)
	}
}

func TestRequiredInventoryLoadsValidInventory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nodes.yml")
	if err := os.WriteFile(path, []byte(`
nodes:
  node-1:
    elastic_eligible: true
    talos_endpoint: 192.0.2.10
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	inv, err := requiredInventory(path)
	if err != nil {
		t.Fatalf("requiredInventory(valid) error = %v, want nil", err)
	}
	if inv == nil || len(inv.Nodes) != 1 {
		t.Fatalf("requiredInventory(valid) = %#v, want one-node inventory", inv)
	}
}
