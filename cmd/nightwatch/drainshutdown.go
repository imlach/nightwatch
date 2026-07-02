package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
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

// storagePlanLine describes which storage-gate backend (if any) will run for
// this node, mirroring operate.lazyStorageGate's own backend selection so the
// plan never lies about what execution will actually do: TrueNAS and Ceph are
// each reported only when their env vars are fully present, and having both
// present is flagged as the same ambiguous-config error the gate itself
// raises at runtime, rather than silently describing just one of them.
func storagePlanLine(node inventory.NodeSpec) string {
	truenas, ceph := operate.TrueNASConfigured(), operate.CephConfigured()
	switch {
	case truenas && ceph:
		return "AMBIGUOUS: both NIGHTWATCH_TRUENAS_* and NIGHTWATCH_CEPH_* are set; configure exactly one storage-gate backend"
	case truenas:
		host, _, _ := operate.TrueNASEnv()
		if id := operate.StorageGateIdentity(node); id != "" {
			return fmt.Sprintf("iscsi gate via %s, match initiator_addr=%s", host, id)
		}
		return fmt.Sprintf("iscsi gate via %s, missing iscsi_initiator_addr", host)
	case ceph:
		host, _, _, _ := operate.CephEnv()
		if id := operate.CephStorageGateIdentity(node); id != "" {
			return fmt.Sprintf("ceph rbd gate via %s, match client_addr=%s", host, id)
		}
		return fmt.Sprintf("ceph rbd gate via %s, missing ceph_client_addr", host)
	default:
		return "skipped (no TrueNAS or Ceph creds in env)"
	}
}

// drainShutdownPlan renders the resolved plan - printed before acting and the
// sole output of --dry-run.
func drainShutdownPlan(invName, kubeName string, node inventory.NodeSpec, c drainShutdownConfig) string {
	storage := storagePlanLine(node)
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
