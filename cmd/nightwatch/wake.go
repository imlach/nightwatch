package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/operate"
)

// wakeConfig is the parsed flag set for the wake subcommand.
type wakeConfig struct {
	kubeconfig, talosconfig                          string
	dryRun, skipGPUWait                              bool
	reachableTimeout, readyTimeout, gpuTimeout, poll time.Duration
}

// runWake brings the ONE named node back into service via the shared operate
// layer: BMC on → Talos reachable → NodeReady → [GPU registered] → uncordon.
func runWake(inv *inventory.Inventory, nodeName string, c wakeConfig) int {
	node, ok := inv.Nodes[nodeName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown node %q\n", nodeName)
		return 1
	}
	kubeName := node.KubeNodeName
	if kubeName == "" {
		kubeName = nodeName
	}
	expectGPU := operate.ExpectGPU(node, c.skipGPUWait)

	fmt.Print(wakePlan(nodeName, kubeName, node, expectGPU, c))
	if c.dryRun {
		fmt.Println("dry-run: not executing")
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	steps, err := operate.Wake(ctx, inv, nodeName, c.operate(), operate.RealBuilder)
	printSteps(steps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wake %s: %v\n", nodeName, err)
		return 1
	}
	fmt.Printf("%s\twake\tresult=ok\n", nodeName)
	return 0
}

func (c wakeConfig) operate() operate.Config {
	return operate.Config{
		Kubeconfig:       c.kubeconfig,
		Talosconfig:      c.talosconfig,
		SkipGPUWait:      c.skipGPUWait,
		ReachableTimeout: c.reachableTimeout,
		ReadyTimeout:     c.readyTimeout,
		GPUTimeout:       c.gpuTimeout,
		Poll:             c.poll,
	}
}

func wakePlan(invName, kubeName string, node inventory.NodeSpec, expectGPU bool, c wakeConfig) string {
	gpu := "no"
	if expectGPU {
		gpu = fmt.Sprintf("yes (%s)", strings.Join(node.GPUs, ", "))
	}
	return fmt.Sprintf("plan wake %s\n"+
		"  kube_node=%s talos=%s bmc=%s/%s\n"+
		"  wait_gpu=%s\n"+
		"  timeouts: reachable=%s ready=%s gpu=%s poll=%s\n",
		invName, kubeName, node.TalosEndpoint, node.BMC.Type, node.BMC.Host,
		gpu, c.reachableTimeout, c.readyTimeout, c.gpuTimeout, c.poll)
}
