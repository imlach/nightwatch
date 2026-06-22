package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/operation"
)

// PowerStarter turns a node on and reads its power state (any bmc.Adapter).
type PowerStarter interface {
	PowerOn(ctx context.Context) bmc.Result
	GetPowerState(ctx context.Context) bmc.Result
}

// TalosReachable reports whether a node's Talos API answers (the talos adapter).
type TalosReachable interface {
	Reachable(ctx context.Context, endpoint string) bool
}

// NodeGater reads readiness / GPU registration and uncordons (satisfied by
// *k8s.Client). Separate from NodeController (drain side) to keep each narrow.
type NodeGater interface {
	IsNodeReady(ctx context.Context, name string) (bool, error)
	NodeHasGPUCapacity(ctx context.Context, name string) (bool, error)
	Uncordon(ctx context.Context, name string) error
}

// PowerOnDeps are the backends the wake sequence drives.
type PowerOnDeps struct {
	Power PowerStarter
	Talos TalosReachable
	Nodes NodeGater
	Now   func() time.Time // injectable clock; defaults to time.Now
}

// PowerOnOptions tunes one wake.
type PowerOnOptions struct {
	TalosEndpoint    string
	ExpectGPU        bool // wait for the GPU to register before uncordoning
	ReachableTimeout time.Duration
	ReadyTimeout     time.Duration
	GPUTimeout       time.Duration
	PollInterval     time.Duration
}

// PowerOn brings a slept node back into service:
//
//	BMC power-on → [wait Talos reachable] → wait NodeReady → [wait GPU registered] → uncordon
//
// If no Talos reachability probe is wired, the Talos wait step is recorded as
// skipped and readiness remains the wake gate.
//
// It records an operation.Step per phase and stops at the first failure WITHOUT
// uncordoning - a node that didn't come up cleanly must stay out of service. The
// uncordon is the last step, so the node only takes work once it's verified up
// (and, for GPU nodes, the device is actually registered - not just NodeReady,
// which can lag per the BMC spike).
func PowerOn(ctx context.Context, node string, deps PowerOnDeps, opts PowerOnOptions) ([]operation.Step, error) {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	var steps []operation.Step
	record := func(name string, err error, details map[string]any) error {
		steps = append(steps, operation.Step{Name: name, Succeeded: err == nil, Message: errMsg(err), Details: details, At: now().UTC()})
		return err
	}

	// Power on via the BMC, unless it already reports on (idempotent wake).
	if state := deps.Power.GetPowerState(ctx); state.OK && state.PowerState == bmc.PowerOn {
		_ = record("bmc-power-on", nil, map[string]any{"already": "on"})
	} else if res := deps.Power.PowerOn(ctx); !res.OK {
		err := fmt.Errorf("bmc power-on failed: %s", res.Error)
		_ = record("bmc-power-on", err, nil)
		return steps, err
	} else {
		_ = record("bmc-power-on", nil, nil)
	}

	if deps.Talos == nil {
		_ = record("wait-talos-reachable", nil, map[string]any{"skipped": "talos reachability unavailable"})
	} else {
		reachable := func(ctx context.Context) (bool, error) { return deps.Talos.Reachable(ctx, opts.TalosEndpoint), nil }
		if err := record("wait-talos-reachable", waitUntil(ctx, opts.ReachableTimeout, opts.PollInterval, "talos reachable", reachable), nil); err != nil {
			return steps, fmt.Errorf("%s: %w", node, err)
		}
	}

	ready := func(ctx context.Context) (bool, error) { return deps.Nodes.IsNodeReady(ctx, node) }
	if err := record("wait-node-ready", waitUntil(ctx, opts.ReadyTimeout, opts.PollInterval, "node ready", ready), nil); err != nil {
		return steps, fmt.Errorf("%s: %w", node, err)
	}

	if opts.ExpectGPU {
		gpu := func(ctx context.Context) (bool, error) { return deps.Nodes.NodeHasGPUCapacity(ctx, node) }
		if err := record("wait-gpu-registered", waitUntil(ctx, opts.GPUTimeout, opts.PollInterval, "gpu registered", gpu), nil); err != nil {
			return steps, fmt.Errorf("%s: %w", node, err)
		}
	} else {
		_ = record("wait-gpu-registered", nil, map[string]any{"skipped": "node not GPU-bearing"})
	}

	if err := record("uncordon", deps.Nodes.Uncordon(ctx, node), nil); err != nil {
		return steps, fmt.Errorf("uncordon %s: %w", node, err)
	}
	return steps, nil
}

// waitUntil polls check until it returns true or the timeout elapses. A check
// error aborts immediately - it is not retried (mirrors the storage gate: an
// error reading state must not be mistaken for the condition being met).
func waitUntil(ctx context.Context, timeout, poll time.Duration, desc string, check func(context.Context) (bool, error)) error {
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
		ok, err := check(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", desc, err)
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%s not satisfied within %s", desc, timeout)
			}
			return fmt.Errorf("%s wait cancelled: %w", desc, ctx.Err()) // parent cancelled (e.g. SIGINT), not a timeout
		case <-time.After(poll):
		}
	}
}
