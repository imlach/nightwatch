// Package operate is the shared assembly+run layer behind both the CLI
// subcommands and the serve HTTP API: it owns the per-node backend wiring
// (k8s + talos + bmc + the TrueNAS iSCSI gate) and the lifecycle invocation,
// so the two front-ends drive one code path. Backend construction is behind a
// Builder seam so handler/unit tests inject fakes without real network.
package operate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/iscsi"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/operation"
	"github.com/imlach/nightwatch/internal/talos"
	"github.com/imlach/nightwatch/internal/truenas"
)

// Config is the assembly knobs shared by drain-shutdown and wake: client config
// paths, the per-phase timeouts, and the per-op toggles. TrueNAS wiring is read
// from the environment (NIGHTWATCH_TRUENAS_*) at build time, matching the CLI.
type Config struct {
	Kubeconfig, Talosconfig string

	// drain-shutdown
	ForceBMCOff                                   bool
	DrainTimeout, StorageTimeout, PowerOffTimeout time.Duration

	// wake
	SkipGPUWait                                bool
	ReachableTimeout, ReadyTimeout, GPUTimeout time.Duration

	Poll time.Duration
}

// NodeOps is the k8s surface both lifecycle paths need (drain side + wake
// side), satisfied by *k8s.Client. As one interface it lets the Builder seam
// inject a single fake covering both ops in tests.
type NodeOps interface {
	lifecycle.NodeController
	lifecycle.NodeGater
}

// Backends are the live clients a lifecycle op drives, plus an optional closer
// run when the op finishes (e.g. the TrueNAS websocket).
type Backends struct {
	Nodes   NodeOps
	Talos   lifecycle.TalosShutdown
	Power   bmc.Adapter
	Storage lifecycle.StorageGate
	Close   func()
}

// Builder assembles the backends for one node. The default (RealBuilder) wires
// the real clients; tests inject a fake to run the lifecycle without network.
type Builder func(ctx context.Context, node inventory.NodeSpec, cfg Config) (Backends, error)

// Lookup resolves a node by inventory name, returning ErrUnknownNode if absent.
func Lookup(inv *inventory.Inventory, name string) (inventory.NodeSpec, error) {
	node, ok := inv.Nodes[name]
	if !ok {
		return inventory.NodeSpec{}, &UnknownNodeError{Name: name}
	}
	return node, nil
}

// UnknownNodeError is returned when a named node is not in the inventory - the
// front-ends map it to a CLI exit 1 / HTTP 404.
type UnknownNodeError struct{ Name string }

func (e *UnknownNodeError) Error() string { return fmt.Sprintf("unknown node %q", e.Name) }

// kubeName is the k8s node name, falling back to the inventory key when unset.
func kubeName(name string, node inventory.NodeSpec) string {
	if node.KubeNodeName != "" {
		return node.KubeNodeName
	}
	return name
}

// DrainShutdown assembles backends for the named node and runs the drain →
// storage gate → OS shutdown → power-off sequence, returning the recorded steps.
func DrainShutdown(ctx context.Context, inv *inventory.Inventory, name string, cfg Config, build Builder) ([]operation.Step, error) {
	node, err := Lookup(inv, name)
	if err != nil {
		return nil, err
	}
	if build == nil {
		build = RealBuilder
	}
	be, err := build(ctx, node, cfg)
	if err != nil {
		return nil, err
	}
	if be.Close != nil {
		defer be.Close()
	}

	deps := lifecycle.DrainShutdownDeps{Nodes: be.Nodes, Talos: be.Talos, Power: be.Power, Storage: be.Storage}
	opts := lifecycle.DrainShutdownOptions{
		TalosEndpoint:   node.TalosEndpoint,
		DrainTimeout:    cfg.DrainTimeout,
		StorageTimeout:  cfg.StorageTimeout,
		PowerOffTimeout: cfg.PowerOffTimeout,
		PollInterval:    cfg.Poll,
		ForceBMCOff:     cfg.ForceBMCOff,
	}
	return lifecycle.DrainShutdown(ctx, kubeName(name, node), deps, opts)
}

// Wake assembles backends for the named node and runs the power-on → reachable
// → ready → [GPU] → uncordon sequence, returning the recorded steps. The GPU
// wait fires only for GPU-bearing nodes that don't set SkipGPUWait.
func Wake(ctx context.Context, inv *inventory.Inventory, name string, cfg Config, build Builder) ([]operation.Step, error) {
	node, err := Lookup(inv, name)
	if err != nil {
		return nil, err
	}
	if build == nil {
		build = RealBuilder
	}
	be, err := build(ctx, node, cfg)
	if err != nil {
		return nil, err
	}
	if be.Close != nil {
		defer be.Close()
	}

	deps := lifecycle.PowerOnDeps{Power: be.Power, Talos: powerOnTalos(be.Talos), Nodes: be.Nodes}
	opts := lifecycle.PowerOnOptions{
		TalosEndpoint:    node.TalosEndpoint,
		ExpectGPU:        ExpectGPU(node, cfg.SkipGPUWait),
		ReachableTimeout: cfg.ReachableTimeout,
		ReadyTimeout:     cfg.ReadyTimeout,
		GPUTimeout:       cfg.GPUTimeout,
		PollInterval:     cfg.Poll,
	}
	return lifecycle.PowerOn(ctx, kubeName(name, node), deps, opts)
}

// ExpectGPU reports whether wake should gate on GPU registration for this node.
func ExpectGPU(node inventory.NodeSpec, skip bool) bool {
	return len(node.GPUs) > 0 && !skip
}

// powerOnTalos narrows the shutdown-capable Talos backend to the Reachable
// check the wake path needs; the real *talos.Client satisfies both.
func powerOnTalos(t lifecycle.TalosShutdown) lifecycle.TalosReachable {
	if r, ok := t.(lifecycle.TalosReachable); ok {
		return r
	}
	return nil
}

// RealBuilder wires the live backends for one node: k8s from kubeconfig, the
// Talos machine API, the per-node BMC adapter, and (when TrueNAS creds are in
// env) the iSCSI session gate bound to the node's explicit inventory identity.
func RealBuilder(ctx context.Context, node inventory.NodeSpec, cfg Config) (Backends, error) {
	storage, closeStorage, err := BuildStorageGate(ctx, StorageGateIdentity(node))
	if err != nil {
		return Backends{}, fmt.Errorf("storage gate: %w", err)
	}
	kc, err := k8s.NewFromKubeconfig(cfg.Kubeconfig)
	if err != nil {
		if closeStorage != nil {
			closeStorage()
		}
		return Backends{}, fmt.Errorf("kube client: %w", err)
	}
	tc, err := talos.New(ctx, cfg.Talosconfig)
	if err != nil {
		if closeStorage != nil {
			closeStorage()
		}
		return Backends{}, fmt.Errorf("talos client: %w", err)
	}
	power, err := bmc.New(node.BMC.Type, node.BMC.Host, node.BMC.Username, node.BMC.Password)
	if err != nil {
		if closeStorage != nil {
			closeStorage()
		}
		_ = tc.Close()
		return Backends{}, fmt.Errorf("bmc: %w", err)
	}
	return Backends{
		Nodes:   kc,
		Talos:   tc,
		Power:   power,
		Storage: storage,
		Close: func() {
			_ = tc.Close()
			if closeStorage != nil {
				closeStorage()
			}
		},
	}, nil
}

// StorageGateIdentity returns the explicit inventory identity used to match the
// node in the TrueNAS iSCSI session table. TrueNAS exposes the stable fabric IP
// as initiator_addr; TalosEndpoint is deliberately not a fallback.
func StorageGateIdentity(node inventory.NodeSpec) string {
	return strings.TrimSpace(node.ISCSIInitiatorAddr)
}

// BuildStorageGate wires the TrueNAS iSCSI session gate from env, bound to the
// node's explicit storage identity. Returns a nil gate when TrueNAS creds are
// unset, so the state machine skips the gate.
func BuildStorageGate(ctx context.Context, gateToken string) (lifecycle.StorageGate, func(), error) {
	host, user, key := TrueNASEnv()
	if host == "" || user == "" || key == "" {
		return nil, nil, nil
	}
	if gateToken == "" {
		return nil, nil, fmt.Errorf("node has no storage gate identity (iscsi_initiator_addr) to match iscsi sessions on")
	}
	tn, err := truenas.New(ctx, host, user, key)
	if err != nil {
		return nil, nil, err
	}
	gate := iscsi.Gate{List: tn.SessionTable}
	sg := lifecycle.StorageGateFunc(func(ctx context.Context, timeout time.Duration) error {
		return gate.WaitClear(ctx, gateToken, timeout)
	})
	return sg, func() { _ = tn.Close() }, nil
}

// TrueNASEnv reads the TrueNAS connection from env. Empty values mean "no gate".
func TrueNASEnv() (host, user, key string) {
	return os.Getenv("NIGHTWATCH_TRUENAS_HOST"),
		os.Getenv("NIGHTWATCH_TRUENAS_USERNAME"),
		os.Getenv("NIGHTWATCH_TRUENAS_API_KEY")
}
