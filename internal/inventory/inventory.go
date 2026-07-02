package inventory

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Inventory struct {
	Nodes map[string]NodeSpec `yaml:"nodes"`
}

type NodeSpec struct {
	ElasticEligible    bool              `yaml:"elastic_eligible"`
	Role               string            `yaml:"role"`
	TalosEndpoint      string            `yaml:"talos_endpoint"`
	KubeNodeName       string            `yaml:"kube_node_name"`
	BMC                BMCConfig         `yaml:"bmc"`
	WakePolicy         map[string]string `yaml:"wake_policy"`
	Labels             map[string]string `yaml:"labels"`
	GPUs               []string          `yaml:"gpus"`
	ISCSIInitiatorAddr string            `yaml:"iscsi_initiator_addr"`
	// CephClientAddr is the node's Ceph client network address exactly as it
	// appears in an `rbd status` watcher entry (e.g. "203.0.113.10"). Like
	// ISCSIInitiatorAddr, this is an explicit, operator-set identity with no
	// derived fallback (see operate.CephStorageGateIdentity): the address a
	// node's RBD client binds to for its watch registration is not reliably
	// derivable from TalosEndpoint or any other field already in this struct,
	// and guessing wrong here means the storage gate silently never clears.
	CephClientAddr string `yaml:"ceph_client_addr"`
}

type BMCConfig struct {
	Type     string `yaml:"type"`
	Host     string `yaml:"host"`
	Username string `yaml:"-"`
	Password string `yaml:"-"`
}

func LoadFile(path string) (*Inventory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(b)
}

func Load(data []byte) (*Inventory, error) {
	var inv Inventory
	if err := yaml.Unmarshal(data, &inv); err != nil {
		return nil, err
	}
	if len(inv.Nodes) == 0 {
		return nil, fmt.Errorf("inventory has no nodes")
	}
	for name, node := range inv.Nodes {
		if err := validateNode(name, node); err != nil {
			return nil, err
		}
	}
	return &inv, nil
}

func (i *Inventory) ElasticNodes() map[string]NodeSpec {
	out := make(map[string]NodeSpec)
	for name, node := range i.Nodes {
		if node.ElasticEligible {
			out[name] = node
		}
	}
	return out
}

func (i *Inventory) ApplyBMCCredentialsFromEnv() {
	defaultUsername := os.Getenv("NIGHTWATCH_BMC_USERNAME")
	defaultPassword := os.Getenv("NIGHTWATCH_BMC_PASSWORD")
	for name, node := range i.Nodes {
		prefix := "NIGHTWATCH_BMC_" + envNodeName(name) + "_"
		if username := os.Getenv(prefix + "USERNAME"); username != "" {
			node.BMC.Username = username
		} else {
			node.BMC.Username = defaultUsername
		}
		if password := os.Getenv(prefix + "PASSWORD"); password != "" {
			node.BMC.Password = password
		} else {
			node.BMC.Password = defaultPassword
		}
		i.Nodes[name] = node
	}
}

func validateNode(name string, node NodeSpec) error {
	if name == "" {
		return fmt.Errorf("node name is empty")
	}
	if node.TalosEndpoint == "" {
		return fmt.Errorf("node %s: talos_endpoint is required", name)
	}
	if node.KubeNodeName == "" {
		return fmt.Errorf("node %s: kube_node_name is required", name)
	}
	if node.BMC.Host == "" {
		return fmt.Errorf("node %s: bmc.host is required", name)
	}
	switch node.BMC.Type {
	case "amt", "idrac", "redfish":
		return nil
	default:
		return fmt.Errorf("node %s: unsupported bmc.type %q", name, node.BMC.Type)
	}
}

func envNodeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}
