// Command nightwatch-operator runs the leader-elected ElasticNode reconciler on
// the management cluster. The manager client talks to the management cluster
// where the CRs live; target-cluster actuation goes through the Backends provider.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	nwv1 "github.com/imlach/nightwatch/api/v1alpha1"
	"github.com/imlach/nightwatch/internal/controller"
	"github.com/imlach/nightwatch/internal/inventory"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = nwv1.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr   = flag.String("metrics-bind-address", ":8080", "metrics endpoint bind address")
		probeAddr     = flag.String("health-probe-bind-address", ":8081", "health/readiness probe bind address")
		enableLeader  = flag.Bool("leader-elect", true, "enable leader election (a mgmt-etcd Lease) for active/standby HA")
		leaderID      = flag.String("leader-elect-id", "nightwatch-operator.nightwatch.imla.ch", "leader-election lease name")
		resync        = flag.Duration("resync", 60*time.Second, "steady level-triggered re-derive cadence")
		targetName    = flag.String("target-cluster", envDefault("NIGHTWATCH_TARGET_CLUSTER", "default"), "target cluster name")
		targetKube    = flag.String("target-kubeconfig", os.Getenv("NIGHTWATCH_TARGET_KUBECONFIG"), "target-cluster kubeconfig (used by the Backends provider, NOT the manager)")
		targetTalos   = flag.String("target-talosconfig", os.Getenv("NIGHTWATCH_TARGET_TALOSCONFIG"), "target-cluster talosconfig")
		inventoryPath = flag.String("inventory", envDefault("NIGHTWATCH_INVENTORY", "/etc/nightwatch/nodes.yml"), "node inventory YAML")
	)
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	// Manager client → MGMT cluster (in-cluster config where the operator runs).
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: *metricsAddr},
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *enableLeader,
		LeaderElectionID:       *leaderID,
		// Failover re-derives level-triggered, so releasing the lease on exit is
		// safe and shortens the standby takeover gap.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Inventory (node identity) - best-effort at boot; the stub provider doesn't
	// require it yet, but load it so a missing file is a loud startup signal.
	inv, invErr := inventory.LoadFile(*inventoryPath)
	if invErr != nil {
		setupLog.Info("inventory not loaded (provider is stubbed; wiring is the next increment)", "path", *inventoryPath, "err", invErr.Error())
	}

	// Backends provider -> target cluster + OOB adapters. Stubbed: cross-cluster
	// wiring is the next increment (see internal/controller/provider.go).
	backends := &controller.ClusterProvider{
		Target: controller.TargetCluster{
			Name:          *targetName,
			Kubeconfig:    *targetKube,
			TalosConfig:   *targetTalos,
			InventoryPath: *inventoryPath,
		},
		Inventory: inv,
	}

	if err := (&controller.Reconciler{
		Client:   mgr.GetClient(),
		Backends: backends,
		Resync:   *resync,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ElasticNode")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting nightwatch-operator", "leaderElection", *enableLeader, "targetCluster", *targetName)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

func envDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
