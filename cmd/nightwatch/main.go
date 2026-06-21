package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	_ "github.com/imlach/nightwatch/internal/bmc/amtwsman" // self-registers the amt driver
	_ "github.com/imlach/nightwatch/internal/bmc/redfish"  // self-registers the redfish/idrac driver
	"github.com/imlach/nightwatch/internal/inventory"
)

func main() {
	invPath := flag.String("inventory", envDefault("NIGHTWATCH_INVENTORY", "/etc/nightwatch/nodes.yml"), "path to node inventory YAML")
	queryBMC := flag.Bool("bmc", false, "query BMC power state during status")
	printRaw := flag.Bool("raw", false, "print raw BMC response on status errors")
	timeout := flag.Duration("timeout", 10*time.Second, "per-BMC request timeout")
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: nightwatch [--inventory path] [--bmc] status | power <on|off|hard-off|reset> <node> | drain-shutdown [flags] <node> | wake [flags] <node> | serve [flags]")
		os.Exit(2)
	}

	switch flag.Arg(0) {
	case "status":
		inv, err := inventory.LoadFile(*invPath)
		if err != nil {
			slog.Error("load inventory", "err", err)
			os.Exit(1)
		}
		if *queryBMC {
			inv.ApplyBMCCredentialsFromEnv()
		}
		printStatus(inv, *queryBMC, *printRaw, *timeout)
	case "power":
		if flag.NArg() < 3 {
			fmt.Fprintln(os.Stderr, "usage: nightwatch power <on|off|hard-off|reset> <node>")
			os.Exit(2)
		}
		inv, err := inventory.LoadFile(*invPath)
		if err != nil {
			slog.Error("load inventory", "err", err)
			os.Exit(1)
		}
		inv.ApplyBMCCredentialsFromEnv()
		os.Exit(runPower(inv, flag.Arg(1), flag.Arg(2), *printRaw, *timeout))
	case "drain-shutdown":
		fs := flag.NewFlagSet("drain-shutdown", flag.ExitOnError)
		kubeconfig := fs.String("kubeconfig", os.Getenv("NIGHTWATCH_KUBECONFIG"), "path to kubeconfig (empty: default rules, then in-cluster)")
		talosconfig := fs.String("talosconfig", os.Getenv("NIGHTWATCH_TALOSCONFIG"), "path to talosconfig")
		forceBMCOff := fs.Bool("force-bmc-off", false, "escalate to a BMC hard-off if graceful shutdown doesn't power the node off")
		dryRun := fs.Bool("dry-run", false, "print the resolved plan and exit without acting")
		drainTimeout := fs.Duration("drain-timeout", 5*time.Minute, "max wait for pods to evict")
		storageTimeout := fs.Duration("storage-timeout", 2*time.Minute, "max wait for the node's iSCSI sessions to clear")
		powerOffTimeout := fs.Duration("poweroff-timeout", 5*time.Minute, "max wait for the BMC to report power off")
		poll := fs.Duration("poll", 5*time.Second, "poll interval for drain/storage/power waits")
		_ = fs.Parse(flag.Args()[1:])
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: nightwatch drain-shutdown [flags] <node>")
			os.Exit(2)
		}
		inv, err := inventory.LoadFile(*invPath)
		if err != nil {
			slog.Error("load inventory", "err", err)
			os.Exit(1)
		}
		inv.ApplyBMCCredentialsFromEnv()
		os.Exit(runDrainShutdown(inv, fs.Arg(0), drainShutdownConfig{
			kubeconfig:      *kubeconfig,
			talosconfig:     *talosconfig,
			forceBMCOff:     *forceBMCOff,
			dryRun:          *dryRun,
			drainTimeout:    *drainTimeout,
			storageTimeout:  *storageTimeout,
			powerOffTimeout: *powerOffTimeout,
			poll:            *poll,
		}))
	case "wake":
		fs := flag.NewFlagSet("wake", flag.ExitOnError)
		kubeconfig := fs.String("kubeconfig", os.Getenv("NIGHTWATCH_KUBECONFIG"), "path to kubeconfig (empty: default rules, then in-cluster)")
		talosconfig := fs.String("talosconfig", os.Getenv("NIGHTWATCH_TALOSCONFIG"), "path to talosconfig")
		dryRun := fs.Bool("dry-run", false, "print the resolved plan and exit without acting")
		skipGPUWait := fs.Bool("skip-gpu-wait", false, "uncordon without waiting for the GPU to register (GPU nodes only)")
		reachableTimeout := fs.Duration("reachable-timeout", 5*time.Minute, "max wait for the Talos API to answer")
		readyTimeout := fs.Duration("ready-timeout", 5*time.Minute, "max wait for the node to go Ready")
		gpuTimeout := fs.Duration("gpu-timeout", 3*time.Minute, "max wait for the GPU to register")
		poll := fs.Duration("poll", 5*time.Second, "poll interval for the reachable/ready/gpu waits")
		_ = fs.Parse(flag.Args()[1:])
		if fs.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: nightwatch wake [flags] <node>")
			os.Exit(2)
		}
		inv, err := inventory.LoadFile(*invPath)
		if err != nil {
			slog.Error("load inventory", "err", err)
			os.Exit(1)
		}
		inv.ApplyBMCCredentialsFromEnv()
		os.Exit(runWake(inv, fs.Arg(0), wakeConfig{
			kubeconfig:       *kubeconfig,
			talosconfig:      *talosconfig,
			dryRun:           *dryRun,
			skipGPUWait:      *skipGPUWait,
			reachableTimeout: *reachableTimeout,
			readyTimeout:     *readyTimeout,
			gpuTimeout:       *gpuTimeout,
			poll:             *poll,
		}))
	case "serve":
		inv, err := inventory.LoadFile(*invPath)
		if err != nil {
			slog.Error("load inventory", "err", err)
			os.Exit(1)
		}
		inv.ApplyBMCCredentialsFromEnv()
		os.Exit(runServe(inv, flag.Args()[1:]))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", flag.Arg(0))
		os.Exit(2)
	}
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func printStatus(inv *inventory.Inventory, queryBMC, printRaw bool, timeout time.Duration) {
	names := make([]string, 0, len(inv.Nodes))
	for name := range inv.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node := inv.Nodes[name]
		fmt.Printf("%s\telastic=%t\tbmc=%s/%s\ttalos=%s", name, node.ElasticEligible, node.BMC.Type, node.BMC.Host, node.TalosEndpoint)
		if queryBMC {
			printBMCPower(name, node, printRaw, timeout)
		}
		fmt.Println()
	}
}

// runPower executes a single BMC power action against one node and returns a
// process exit code (0 ok, 1 error). A node must be named explicitly; there is
// no broadcast form, to keep a sharp tool from acting on the whole fleet.
func runPower(inv *inventory.Inventory, action, nodeName string, printRaw bool, timeout time.Duration) int {
	node, ok := inv.Nodes[nodeName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown node %q\n", nodeName)
		return 1
	}
	client, err := bmc.New(node.BMC.Type, node.BMC.Host, node.BMC.Username, node.BMC.Password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "node %s: unsupported bmc.type %q for power actions\n", nodeName, node.BMC.Type)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var result bmc.Result
	switch action {
	case "on":
		result = client.PowerOn(ctx)
	case "off", "soft-off":
		result = client.SoftOff(ctx)
	case "hard-off":
		result = client.HardOff(ctx)
	case "reset", "cycle":
		result = client.Reset(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown power action %q (want on|off|hard-off|reset)\n", action)
		return 2
	}

	if result.OK {
		fmt.Printf("%s\tpower_action=%s\tresult=ok\tintended_state=%s\n", nodeName, action, result.PowerState)
		return 0
	}
	fmt.Printf("%s\tpower_action=%s\tresult=error\tbmc_error=%q", nodeName, action, result.Error)
	if printRaw && result.Raw != "" {
		fmt.Printf("\tbmc_raw=%q", result.Raw)
	}
	fmt.Println()
	return 1
}

func printBMCPower(name string, node inventory.NodeSpec, printRaw bool, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client, err := bmc.New(node.BMC.Type, node.BMC.Host, node.BMC.Username, node.BMC.Password)
	if err != nil {
		fmt.Printf("\tbmc_power=unsupported\tbmc_error=%q", "unsupported bmc.type "+node.BMC.Type+" for "+name)
		return
	}
	result := client.GetPowerState(ctx)
	if result.OK {
		fmt.Printf("\tbmc_power=%s", result.PowerState)
		return
	}
	fmt.Printf("\tbmc_power=unknown\tbmc_error=%q", result.Error)
	if printRaw && result.Raw != "" {
		fmt.Printf("\tbmc_raw=%q", result.Raw)
	}
}
