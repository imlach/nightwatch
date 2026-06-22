package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/operate"
	"github.com/imlach/nightwatch/internal/operation"
)

// drainShutdownConfig is the parsed flag set for the drain-shutdown subcommand.
type drainShutdownConfig struct {
	kubeconfig, talosconfig      string
	forceBMCOff, dryRun          bool
	drainTimeout, storageTimeout time.Duration
	powerOffTimeout, poll        time.Duration
}

// runDrainShutdown prints the resolved plan and (unless --dry-run) runs the
// drain-shutdown sequence for the ONE named node via the shared operate layer.
// There is no fleet/broadcast form - this is a sharp, single-node tool.
func runDrainShutdown(inv *inventory.Inventory, nodeName string, c drainShutdownConfig) int {
	node, ok := inv.Nodes[nodeName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown node %q\n", nodeName)
		return 1
	}
	kubeName := node.KubeNodeName
	if kubeName == "" {
		kubeName = nodeName
	}

	fmt.Print(drainShutdownPlan(nodeName, kubeName, node, c))
	if c.dryRun {
		fmt.Println("dry-run: not executing")
		return 0
	}

	// Long-running drain/poll loops honor ctx, so Ctrl-C / SIGTERM cancels
	// cleanly - leaving the node cordoned, never half-powered silently.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	steps, err := operate.DrainShutdown(ctx, inv, nodeName, c.operate(), operate.RealBuilder)
	printSteps(steps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "drain-shutdown %s: %v\n", nodeName, err)
		return 1
	}
	fmt.Printf("%s\tdrain-shutdown\tresult=ok\n", nodeName)
	return 0
}

func (c drainShutdownConfig) operate() operate.Config {
	return operate.Config{
		Kubeconfig:      c.kubeconfig,
		Talosconfig:     c.talosconfig,
		ForceBMCOff:     c.forceBMCOff,
		DrainTimeout:    c.drainTimeout,
		StorageTimeout:  c.storageTimeout,
		PowerOffTimeout: c.powerOffTimeout,
		Poll:            c.poll,
	}
}

// buildStorageGate delegates to the shared operate layer (same env-driven
// iSCSI gate the executor wires) - kept here so the CLI plan/test surface is
// stable.
func buildStorageGate(ctx context.Context, gateToken string) (lifecycle.StorageGate, func(), error) {
	return operate.BuildStorageGate(ctx, gateToken)
}

// drainShutdownPlan renders the resolved plan - printed before acting and the
// sole output of --dry-run.
func drainShutdownPlan(invName, kubeName string, node inventory.NodeSpec, c drainShutdownConfig) string {
	storage := "skipped (no TrueNAS creds in env)"
	if host, user, key := operate.TrueNASEnv(); host != "" && user != "" && key != "" {
		if id := operate.StorageGateIdentity(node); id != "" {
			storage = fmt.Sprintf("iscsi gate via %s, match initiator_addr=%s", host, id)
		} else {
			storage = fmt.Sprintf("iscsi gate via %s, missing iscsi_initiator_addr", host)
		}
	}
	return fmt.Sprintf("plan drain-shutdown %s\n"+
		"  kube_node=%s talos=%s bmc=%s/%s\n"+
		"  storage=%s force_bmc_off=%t\n"+
		"  timeouts: drain=%s storage=%s poweroff=%s poll=%s\n",
		invName, kubeName, node.TalosEndpoint, node.BMC.Type, node.BMC.Host,
		storage, c.forceBMCOff, c.drainTimeout, c.storageTimeout, c.powerOffTimeout, c.poll)
}

func printSteps(steps []operation.Step) {
	for _, s := range steps {
		status := "ok"
		if !s.Succeeded {
			status = "FAILED"
		}
		line := fmt.Sprintf("  step=%s\t%s", s.Name, status)
		if s.Message != "" {
			line += fmt.Sprintf("\tmsg=%q", s.Message)
		}
		fmt.Println(line)
	}
}
