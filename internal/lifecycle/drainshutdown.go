// Package lifecycle holds Nightwatch's node-lifecycle state machines. They
// orchestrate per-capability backends behind narrow, swappable interfaces and
// record a Step per phase, so an operation is resumable/auditable and the logic
// is unit-testable without a live cluster.
//
// Backends are modular by design:
//   - Power (BMC): any bmc.Adapter - AMT (internal/bmc/amtwsman), Redfish
//     (iDRAC/iLO), IPMI - selected per node by inventory bmc.type.
//   - StorageGate: any "wait until this node's volumes are detached" backend -
//     iSCSI session gate (internal/iscsi) today; Ceph RBD watcher/map checks,
//     NFS, etc. slot in behind the same interface.
//   - NodeController / TalosShutdown: the k8s drain and node-OS shutdown.
package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/operation"
)

// NodeController cordons and drains a node (satisfied by *k8s.Client).
type NodeController interface {
	Cordon(ctx context.Context, node string) error
	Drain(ctx context.Context, node string, opts k8s.DrainOptions) error
}

// TalosShutdown issues a graceful OS-level shutdown (satisfied by the Talos
// adapter, wired separately).
type TalosShutdown interface {
	Shutdown(ctx context.Context, endpoint string) error
}

// PowerController reads power state and, as a fallback, forces it off. Satisfied
// by any bmc.Adapter (AMT/Redfish/IPMI), so the BMC backend is swappable.
type PowerController interface {
	GetPowerState(ctx context.Context) bmc.Result
	HardOff(ctx context.Context) bmc.Result
}

// StorageGate blocks until a node's backing storage is safely detached - the
// hard pre-power-off data-safety barrier (R5). It is backend-agnostic and bound
// to one node at wiring time (e.g. an iSCSI gate bound to the node's initiator
// IQN, or a Ceph gate bound to its RBD client). nil means "no storage gate".
type StorageGate interface {
	WaitDetached(ctx context.Context, timeout time.Duration) error
}

// StorageGateFunc adapts a func to a StorageGate - used to bind a node-agnostic
// backend (e.g. iscsi.Gate.WaitClear) to a specific node's storage identity.
type StorageGateFunc func(ctx context.Context, timeout time.Duration) error

func (f StorageGateFunc) WaitDetached(ctx context.Context, timeout time.Duration) error {
	return f(ctx, timeout)
}

// DrainShutdownDeps are the backends the state machine drives. Storage may be nil
// (node has no detach-sensitive storage / not yet measured), which skips the gate.
type DrainShutdownDeps struct {
	Nodes   NodeController
	Talos   TalosShutdown
	Power   PowerController
	Storage StorageGate
	Now     func() time.Time // injectable clock; defaults to time.Now
}

// DrainShutdownOptions tunes one drain-shutdown.
type DrainShutdownOptions struct {
	TalosEndpoint   string
	DrainTimeout    time.Duration
	StorageTimeout  time.Duration
	PowerOffTimeout time.Duration
	PollInterval    time.Duration
	ForceBMCOff     bool // escalate to a BMC hard-off if the graceful shutdown doesn't power it down
}

// DrainShutdown runs the gated sequence:
//
//	cordon → drain (wait for pods gone) → [storage detach gate] →
//	OS graceful shutdown → wait for BMC=off → [optional BMC hard-off]
//
// It records an operation.Step per phase and stops at the first failure, leaving
// the node cordoned (a partially-drained node must not be silently powered on).
// The storage gate is the hard data-safety barrier before power is pulled (R5);
// the BMC hard-off is an explicit fallback only, never the normal path.
func DrainShutdown(ctx context.Context, node string, deps DrainShutdownDeps, opts DrainShutdownOptions) ([]operation.Step, error) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	var steps []operation.Step
	record := func(name string, err error, details map[string]any) error {
		steps = append(steps, operation.Step{Name: name, Succeeded: err == nil, Message: errMsg(err), Details: details, At: now().UTC()})
		return err
	}

	if err := record("cordon", deps.Nodes.Cordon(ctx, node), nil); err != nil {
		return steps, fmt.Errorf("cordon %s: %w", node, err)
	}

	drainOpts := k8s.DrainOptions{Timeout: opts.DrainTimeout, PollInterval: opts.PollInterval}
	if err := record("drain", deps.Nodes.Drain(ctx, node, drainOpts), nil); err != nil {
		return steps, fmt.Errorf("drain %s: %w", node, err)
	}

	if deps.Storage != nil {
		if err := record("storage-gate", deps.Storage.WaitDetached(ctx, opts.StorageTimeout), nil); err != nil {
			return steps, fmt.Errorf("storage gate %s: %w", node, err)
		}
	} else {
		_ = record("storage-gate", nil, map[string]any{"skipped": "no storage gate configured"})
	}

	if err := record("talos-shutdown", deps.Talos.Shutdown(ctx, opts.TalosEndpoint), nil); err != nil {
		return steps, fmt.Errorf("talos shutdown %s: %w", node, err)
	}

	offErr := waitPowerOff(ctx, deps.Power, opts.PowerOffTimeout, opts.PollInterval)
	_ = record("wait-power-off", offErr, nil)
	if offErr == nil {
		return steps, nil
	}
	if !opts.ForceBMCOff {
		return steps, fmt.Errorf("%s did not power off after graceful shutdown: %w", node, offErr)
	}

	// Explicit fallback: force the node off via the BMC, then re-confirm.
	if res := deps.Power.HardOff(ctx); !res.OK {
		err := fmt.Errorf("bmc hard-off failed: %s", res.Error)
		_ = record("bmc-hard-off", err, nil)
		return steps, err
	}
	_ = record("bmc-hard-off", nil, nil)
	if err := record("wait-power-off-after-hardoff", waitPowerOff(ctx, deps.Power, opts.PowerOffTimeout, opts.PollInterval), nil); err != nil {
		return steps, fmt.Errorf("%s still on after hard-off: %w", node, err)
	}
	return steps, nil
}

// waitPowerOff polls the BMC until it reports PowerOff or the timeout elapses.
func waitPowerOff(ctx context.Context, power PowerController, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = 3 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if err := ctx.Err(); err != nil { // honor an already-done parent before polling
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		res := power.GetPowerState(ctx)
		if res.PowerState == bmc.PowerOff {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("power still %s after %s: %w", res.PowerState, timeout, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
