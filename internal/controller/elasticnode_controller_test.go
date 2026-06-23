package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nwv1 "github.com/imlach/nightwatch/api/v1alpha1"
	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
)

// fakeWorld is the live target-cluster + BMC state the lifecycle state machines
// drive against. It is the "world" the reconciler observes - never the CR status.
type fakeWorld struct {
	on         bool
	powerErr   bool
	cordoned   bool
	drainCalls int
	powerCalls int
}

// fakeWorld satisfies every lifecycle backend interface, mutating itself so the
// real DrainShutdown / PowerOn converge it (idempotent + monotonic).
func (w *fakeWorld) GetPowerState(context.Context) bmc.Result {
	if w.powerErr {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: "bmc unavailable"}
	}
	st := bmc.PowerOff
	if w.on {
		st = bmc.PowerOn
	}
	return bmc.Result{OK: true, PowerState: st}
}
func (w *fakeWorld) HardOff(context.Context) bmc.Result {
	w.on = false
	return bmc.Result{OK: true, PowerState: bmc.PowerOff}
}
func (w *fakeWorld) PowerOn(context.Context) bmc.Result {
	w.powerCalls++
	w.on = true
	return bmc.Result{OK: true, PowerState: bmc.PowerOn}
}

func (w *fakeWorld) Cordon(context.Context, string) error { w.cordoned = true; return nil }
func (w *fakeWorld) Drain(context.Context, string, k8s.DrainOptions) error {
	w.drainCalls++
	return nil
}

func (w *fakeWorld) Shutdown(context.Context, string) error { w.on = false; return nil } // OS shutdown powers it off
func (w *fakeWorld) Reachable(context.Context, string) bool { return w.on }

func (w *fakeWorld) IsNodeReady(context.Context, string) (bool, error)        { return w.on, nil }
func (w *fakeWorld) NodeHasGPUCapacity(context.Context, string) (bool, error) { return w.on, nil }
func (w *fakeWorld) IsNodeSchedulable(context.Context, string) (bool, error)  { return !w.cordoned, nil }
func (w *fakeWorld) Uncordon(context.Context, string) error                   { w.cordoned = false; return nil }

// fakeBackends hands the reconciler a NodeBackends wired to the shared world.
type fakeBackends struct{ w *fakeWorld }

func (b *fakeBackends) BackendsFor(ctx context.Context, ref BackendRef) (*NodeBackends, error) {
	fast := time.Millisecond
	return &NodeBackends{
		Power:       b.w,
		Gater:       b.w,
		Drain:       lifecycle.DrainShutdownDeps{Nodes: b.w, Talos: b.w, Power: b.w},
		PowerOn:     lifecycle.PowerOnDeps{Power: b.w, Talos: b.w, Nodes: b.w},
		DrainOpts:   lifecycle.DrainShutdownOptions{PollInterval: fast, PowerOffTimeout: time.Second, TalosEndpoint: "192.0.2.10"},
		PowerOnOpts: lifecycle.PowerOnOptions{PollInterval: fast, ReachableTimeout: time.Second, ReadyTimeout: time.Second, TalosEndpoint: "192.0.2.10"},
	}, nil
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := nwv1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newReconciler(t *testing.T, en *nwv1.ElasticNode, w *fakeWorld) (*Reconciler, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(en).
		WithStatusSubresource(&nwv1.ElasticNode{}).
		Build()
	r := &Reconciler{Client: c, Backends: &fakeBackends{w: w}, Resync: time.Minute}
	return r, c
}

func reconcileOnce(t *testing.T, r *Reconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: "nightwatch"}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func getEN(t *testing.T, c client.Client, name string) *nwv1.ElasticNode {
	t.Helper()
	var en nwv1.ElasticNode
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "nightwatch"}, &en); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return &en
}

func newEN(name string, desired nwv1.PowerState) *nwv1.ElasticNode {
	return &nwv1.ElasticNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "nightwatch", Generation: 1},
		Spec:       nwv1.ElasticNodeSpec{DesiredPower: desired},
	}
}

// Off on an on-node → DrainShutdown runs, node ends off, status.phase=Off.
func TestReconcile_DesiredOff_DrainsAndPowersOff(t *testing.T) {
	w := &fakeWorld{on: true}
	en := newEN("node-1", nwv1.PowerOff)
	r, c := newReconciler(t, en, w)

	reconcileOnce(t, r, "node-1")

	if w.on {
		t.Fatalf("node should be powered off")
	}
	if w.drainCalls != 1 {
		t.Fatalf("DrainShutdown should have drained once, got %d", w.drainCalls)
	}
	if !w.cordoned {
		t.Fatalf("node should be cordoned after drain-shutdown")
	}
	got := getEN(t, c, "node-1")
	if got.Status.Phase != nwv1.PhaseOff {
		t.Fatalf("phase = %q, want Off", got.Status.Phase)
	}
	if got.Status.ObservedPower != nwv1.PowerOff {
		t.Fatalf("observedPower = %q, want Off", got.Status.ObservedPower)
	}
}

// On on an off-node → PowerOn runs, node ends on + uncordoned, status.phase=Ready.
func TestReconcile_DesiredOn_PowersOnAndUncordons(t *testing.T) {
	w := &fakeWorld{on: false, cordoned: true}
	en := newEN("node-1", nwv1.PowerOn)
	r, c := newReconciler(t, en, w)

	reconcileOnce(t, r, "node-1")

	if !w.on {
		t.Fatalf("node should be powered on")
	}
	if w.powerCalls != 1 {
		t.Fatalf("PowerOn should have powered on once, got %d", w.powerCalls)
	}
	if w.cordoned {
		t.Fatalf("node should be uncordoned after power-on")
	}
	got := getEN(t, c, "node-1")
	if got.Status.Phase != nwv1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if !got.Status.NodeReady {
		t.Fatalf("status.nodeReady should be true")
	}
}

// On on an already-powered Ready node that is still cordoned must drive the
// idempotent wake path so the final uncordon step repairs it without a power
// cycle.
func TestReconcile_DesiredOn_UncordonsReadyButCordoned(t *testing.T) {
	w := &fakeWorld{on: true, cordoned: true}
	en := newEN("node-1", nwv1.PowerOn)
	r, c := newReconciler(t, en, w)

	reconcileOnce(t, r, "node-1")

	if w.powerCalls != 0 {
		t.Fatalf("already-on node should not be power-cycled, got %d power calls", w.powerCalls)
	}
	if w.cordoned {
		t.Fatalf("Ready desired-on node should be uncordoned")
	}
	got := getEN(t, c, "node-1")
	if got.Status.Phase != nwv1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
}

// Level-triggered recovery: a second reconcile with desired already satisfied is
// a no-op - it does NOT re-drive (idempotent), proving the loop reads the world,
// not its own status. Asserted for both directions.
func TestReconcile_LevelTriggered_NoopWhenConverged(t *testing.T) {
	t.Run("on-converged", func(t *testing.T) {
		w := &fakeWorld{on: true}
		en := newEN("node-1", nwv1.PowerOn)
		r, _ := newReconciler(t, en, w)

		reconcileOnce(t, r, "node-1") // already on+ready → must not power-cycle
		reconcileOnce(t, r, "node-1")
		if w.powerCalls != 0 {
			t.Fatalf("converged-on must not call PowerOn, got %d", w.powerCalls)
		}
	})
	t.Run("off-converged", func(t *testing.T) {
		w := &fakeWorld{on: false}
		en := newEN("node-1", nwv1.PowerOff)
		r, _ := newReconciler(t, en, w)

		reconcileOnce(t, r, "node-1") // already off → must not drain again
		reconcileOnce(t, r, "node-1")
		if w.drainCalls != 0 {
			t.Fatalf("converged-off must not call DrainShutdown, got %d", w.drainCalls)
		}
	})
}

// If BMC state cannot be read but the target node is already Ready, desired=On
// is satisfied. The controller must not issue a redundant power-on just because
// the out-of-band read is unavailable.
func TestReconcile_DesiredOn_BMCUnknownButNodeReady(t *testing.T) {
	w := &fakeWorld{on: true, powerErr: true}
	en := newEN("node-1", nwv1.PowerOn)
	r, c := newReconciler(t, en, w)

	reconcileOnce(t, r, "node-1")

	if w.powerCalls != 0 {
		t.Fatalf("ready node with unknown BMC power must not call PowerOn, got %d", w.powerCalls)
	}
	got := getEN(t, c, "node-1")
	if got.Status.Phase != nwv1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if got.Status.ObservedPower != nwv1.PowerOn {
		t.Fatalf("observedPower = %q, want On inferred from Ready node", got.Status.ObservedPower)
	}
	if !got.Status.NodeReady {
		t.Fatalf("status.nodeReady should be true")
	}
}

// Status corruption must not change behaviour: even if status claims Ready, an
// off-node with desired=On is still driven on. Recovery is from the world, not
// status.
func TestReconcile_IgnoresStaleStatus(t *testing.T) {
	w := &fakeWorld{on: false, cordoned: true}
	en := newEN("node-1", nwv1.PowerOn)
	en.Status = nwv1.ElasticNodeStatus{Phase: nwv1.PhaseReady, ObservedPower: nwv1.PowerOn, NodeReady: true}
	r, _ := newReconciler(t, en, w)

	reconcileOnce(t, r, "node-1")

	if !w.on || w.powerCalls != 1 {
		t.Fatalf("stale Ready status must not suppress power-on (on=%v calls=%d)", w.on, w.powerCalls)
	}
}
