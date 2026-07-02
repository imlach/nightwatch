package inventory

import "testing"

func TestLoadInventory(t *testing.T) {
	inv, err := Load([]byte(`
nodes:
  node-1:
    elastic_eligible: true
    role: gpu-worker-small
    talos_endpoint: "192.0.2.10"
    iscsi_initiator_addr: "198.51.100.10"
    ceph_client_addr: "203.0.113.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
    wake_policy: {quiet_mode: eligible}
    labels: {example.com/gpu-model: "example-gpu"}
    gpus: ["example-gpu"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := len(inv.ElasticNodes()); got != 1 {
		t.Fatalf("ElasticNodes() len = %d, want 1", got)
	}
	node := inv.Nodes["node-1"]
	if node.BMC.Type != "amt" || node.BMC.Host != "192.0.2.1" {
		t.Fatalf("unexpected BMC config: %+v", node.BMC)
	}
	if node.ISCSIInitiatorAddr != "198.51.100.10" {
		t.Fatalf("iscsi_initiator_addr = %q, want 198.51.100.10", node.ISCSIInitiatorAddr)
	}
	if node.CephClientAddr != "203.0.113.10" {
		t.Fatalf("ceph_client_addr = %q, want 203.0.113.10", node.CephClientAddr)
	}
}

func TestLoadRejectsUnsupportedBMC(t *testing.T) {
	_, err := Load([]byte(`
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: wsman, host: "192.0.2.1"}
`))
	if err == nil {
		t.Fatal("Load() error = nil, want unsupported bmc.type error")
	}
}

func TestLoadAcceptsRedfish(t *testing.T) {
	inv, err := Load([]byte(`
nodes:
  node-3:
    talos_endpoint: "192.0.2.12"
    kube_node_name: node-3
    bmc: {type: redfish, host: "192.0.2.3"}
`))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for redfish bmc.type", err)
	}
	if got := inv.Nodes["node-3"].BMC.Type; got != "redfish" {
		t.Fatalf("node-3 bmc.type = %q, want redfish", got)
	}
}

func TestApplyBMCCredentialsFromEnv(t *testing.T) {
	t.Setenv("NIGHTWATCH_BMC_USERNAME", "default-user")
	t.Setenv("NIGHTWATCH_BMC_PASSWORD", "default-password")
	t.Setenv("NIGHTWATCH_BMC_NODE_1_USERNAME", "node-user")
	t.Setenv("NIGHTWATCH_BMC_NODE_1_PASSWORD", "node-password")
	inv, err := Load([]byte(`
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
  node-2:
    talos_endpoint: "192.0.2.11"
    kube_node_name: node-2
    bmc: {type: amt, host: "192.0.2.2"}
`))
	if err != nil {
		t.Fatal(err)
	}
	inv.ApplyBMCCredentialsFromEnv()
	if got := inv.Nodes["node-1"].BMC.Username; got != "node-user" {
		t.Fatalf("node-1 username = %q, want node-user", got)
	}
	if got := inv.Nodes["node-1"].BMC.Password; got != "node-password" {
		t.Fatalf("node-1 password = %q, want node-password", got)
	}
	if got := inv.Nodes["node-2"].BMC.Username; got != "default-user" {
		t.Fatalf("node-2 username = %q, want default-user", got)
	}
}
