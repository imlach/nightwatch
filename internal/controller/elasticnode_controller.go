// Package controller holds the ElasticNode reconciler - Nightwatch's
// level-triggered control loop.
//
// Two kube clients, never conflated:
//   - the controller-runtime manager client (this Reconciler's r.Client) talks to
//     the MGMT cluster, where the ElasticNode CRs live;
//   - a separate target-cluster client, reached only via the injected Backends
//     provider, does the cordon/drain/ready/GPU work on the nodes themselves.
//
// Level-triggered invariant: Reconcile recovers from spec.desiredPower + the live
// world (BMC power + target node Ready/GPU), NEVER from status. status is written
// on transitions for observability only.
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nwv1 "github.com/imlach/nightwatch/api/v1alpha1"
	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/lifecycle"
)

// Reconciler converges each ElasticNode toward its desired power state.
type Reconciler struct {
	client.Client // mgmt-cluster client (where ElasticNode CRs live)

	// Backends builds the target-cluster + OOB lifecycle deps per node. This is
	// the only path to the *target* cluster - the mgmt client never touches it.
	Backends Backends

	// Resync is the steady re-derive cadence; defaults to defaultResync.
	Resync time.Duration

	// Now is an injectable clock for transition timestamps (defaults to time.Now).
	Now func() time.Time
}

// +kubebuilder:rbac:groups=nightwatch.imla.ch,resources=elasticnodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=nightwatch.imla.ch,resources=elasticnodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=nightwatch.imla.ch,resources=elasticnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives one ElasticNode toward spec.desiredPower from the live world.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var en nwv1.ElasticNode
	if err := r.Get(ctx, req.NamespacedName, &en); err != nil {
		// Gone → nothing to converge; drop it. Transient errors requeue.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desired := en.Spec.DesiredPower
	if desired == "" {
		desired = nwv1.PowerOn // fail-safe: unknown intent keeps the node in service
	}

	node := en.Name // CR name is the node identity; the provider maps it onward

	be, err := r.Backends.BackendsFor(ctx, BackendRef{Node: node, Cluster: en.Spec.ClusterRef})
	if err != nil {
		// Can't reach the actuators (kubeconfig/inventory/BMC) - observe nothing,
		// surface Error, requeue. NEVER fall back to status to decide.
		return r.finish(ctx, &en, nwv1.PhaseError, "", false, false,
			fmt.Errorf("backends for %s: %w", node, err))
	}
	if be.Close != nil {
		defer be.Close() // release the TrueNAS websocket etc. after this reconcile
	}

	// Observe the live world. The BMC power read is authoritative for up/off; the
	// target node Ready/GPU are best-effort (a down node errors -> treated false).
	power, powerKnown := observePower(ctx, be.Power)
	ready, gpu := observeNode(ctx, be.Gater, node)

	// Direction is decided from desired vs observed, NOT from status.
	switch desired {
	case nwv1.PowerOff:
		// Converged when the BMC reports off. If we can't read power, re-drive
		// drain-shutdown anyway - it is idempotent and stops at the first failure.
		if powerKnown && power == bmc.PowerOff {
			return r.finish(ctx, &en, nwv1.PhaseOff, bmc.PowerOff, false, false, nil)
		}
		lg.Info("converging to Off", "node", node, "observedPower", power)
		if _, dErr := lifecycle.DrainShutdown(ctx, node, be.Drain, be.DrainOpts); dErr != nil {
			return r.finish(ctx, &en, nwv1.PhaseDraining, power, ready, gpu,
				fmt.Errorf("drain-shutdown %s: %w", node, dErr))
		}
		// Re-read so status reflects the world we just changed.
		power, _ = observePower(ctx, be.Power)
		return r.finish(ctx, &en, nwv1.PhaseOff, power, false, false, nil)

	default: // PowerOn (the fail-safe target)
		// Converged when the node is on AND Ready (and GPU-registered if expected).
		if powerKnown && power == bmc.PowerOn && ready && (!be.PowerOnOpts.ExpectGPU || gpu) {
			return r.finish(ctx, &en, nwv1.PhaseReady, bmc.PowerOn, true, gpu, nil)
		}
		// Some BMCs are less reliable than the target-cluster signal. If the BMC
		// read is unavailable but Kubernetes already proves the node is in service,
		// do not issue a redundant power-on that can fail or flap firmware state.
		if !powerKnown && ready && (!be.PowerOnOpts.ExpectGPU || gpu) {
			return r.finish(ctx, &en, nwv1.PhaseReady, bmc.PowerOn, true, gpu, nil)
		}
		lg.Info("converging to On", "node", node, "observedPower", power, "ready", ready)
		if _, pErr := lifecycle.PowerOn(ctx, node, be.PowerOn, be.PowerOnOpts); pErr != nil {
			return r.finish(ctx, &en, nwv1.PhasePoweringOn, power, ready, gpu,
				fmt.Errorf("power-on %s: %w", node, pErr))
		}
		power, _ = observePower(ctx, be.Power)
		ready, gpu = observeNode(ctx, be.Gater, node)
		return r.finish(ctx, &en, nwv1.PhaseReady, power, ready, gpu, nil)
	}
}

// observeNode reads Ready + GPU from the target cluster. Errors (node down /
// unreachable) collapse to false - the reconciler treats "can't confirm Ready"
// as not-ready and re-drives, rather than trusting a stale belief.
func observeNode(ctx context.Context, g lifecycle.NodeGater, node string) (ready, gpu bool) {
	if g == nil {
		return false, false
	}
	if rdy, err := g.IsNodeReady(ctx, node); err == nil {
		ready = rdy
	}
	if has, err := g.NodeHasGPUCapacity(ctx, node); err == nil {
		gpu = has
	}
	return ready, gpu
}

// powerEnum maps a BMC power reading to the API enum for status only.
func powerEnum(p bmc.PowerState) nwv1.PowerState {
	switch p {
	case bmc.PowerOn:
		return nwv1.PowerOn
	case bmc.PowerOff:
		return nwv1.PowerOff
	default:
		return ""
	}
}

// finish writes status (best-effort, on transition only) and returns the steady
// requeue. opErr, if non-null, is returned so controller-runtime backs off and
// retries - but status is still recorded for observability first.
func (r *Reconciler) finish(ctx context.Context, en *nwv1.ElasticNode, phase nwv1.Phase, power bmc.PowerState, ready, gpu bool, opErr error) (ctrl.Result, error) {
	r.writeStatus(ctx, en, phase, powerEnum(power), ready, gpu, opErr)
	if opErr != nil {
		return ctrl.Result{}, opErr // requeue with backoff
	}
	return ctrl.Result{RequeueAfter: r.resync()}, nil
}

// writeStatus patches status only when something changed (transition-only), so a
// steady-state reconcile is a true no-op and doesn't churn the API. This is
// observability, never read back for control flow.
func (r *Reconciler) writeStatus(ctx context.Context, en *nwv1.ElasticNode, phase nwv1.Phase, power nwv1.PowerState, ready, gpu bool, opErr error) {
	lg := log.FromContext(ctx)
	now := r.now()

	want := en.Status.DeepCopy()
	if want.Phase != phase {
		want.LastTransitionTime = &metav1.Time{Time: now}
	}
	want.Phase = phase
	want.ObservedPower = power
	want.NodeReady = ready
	want.GPURegistered = gpu
	want.ObservedGeneration = en.Generation

	setCondition(want, nwv1.ConditionReady, ready, en.Generation, now,
		readyReason(phase, opErr), readyMessage(phase, opErr))
	converged := opErr == nil && (phase == nwv1.PhaseReady || phase == nwv1.PhaseOff)
	setCondition(want, nwv1.ConditionConverged, converged, en.Generation, now,
		convergedReason(phase, opErr), convergedMessage(en.Spec.DesiredPower, opErr))

	if equality.Semantic.DeepEqual(&en.Status, want) {
		return // no-op: idempotent steady state, no API write
	}

	en.Status = *want
	if err := r.Status().Update(ctx, en); err != nil {
		if apierrors.IsConflict(err) {
			return // a fresher reconcile will rewrite it; status is best-effort
		}
		lg.Error(err, "status update", "node", en.Name)
	}
}

func setCondition(s *nwv1.ElasticNodeStatus, condType string, ok bool, gen int64, now time.Time, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	cond := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.Time{Time: now},
		Reason:             reason,
		Message:            msg,
	}
	for i := range s.Conditions {
		if s.Conditions[i].Type != condType {
			continue
		}
		if s.Conditions[i].Status == cond.Status {
			cond.LastTransitionTime = s.Conditions[i].LastTransitionTime // preserve on no flip
		}
		s.Conditions[i] = cond
		return
	}
	s.Conditions = append(s.Conditions, cond)
}

func readyReason(phase nwv1.Phase, opErr error) string {
	if opErr != nil {
		return "OperationFailed"
	}
	return string(phase)
}

func readyMessage(phase nwv1.Phase, opErr error) string {
	if opErr != nil {
		return opErr.Error()
	}
	return fmt.Sprintf("phase=%s", phase)
}

func convergedReason(phase nwv1.Phase, opErr error) string {
	if opErr != nil {
		return "Converging"
	}
	return string(phase)
}

func convergedMessage(desired nwv1.PowerState, opErr error) string {
	if opErr != nil {
		return opErr.Error()
	}
	if desired == "" {
		desired = nwv1.PowerOn
	}
	return fmt.Sprintf("converged to desiredPower=%s", desired)
}

func (r *Reconciler) resync() time.Duration {
	if r.Resync > 0 {
		return r.Resync
	}
	return defaultResync
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager wires the reconciler to watch ElasticNode on the mgmt cluster.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nwv1.ElasticNode{}).
		Named("elasticnode").
		Complete(r)
}
