//go:build integration

package sim_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/imlach/nightwatch/internal/bmc/amtwsman"
	"github.com/imlach/nightwatch/internal/iscsi"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/sim"
	"github.com/imlach/nightwatch/internal/truenas"
)

// simTalos models the Talos OS-level shutdown: when DrainShutdown issues it the
// host powers off, which the AMT sim then reports. Wired in-process because a
// Talos gRPC sim needs mTLS cert machinery (deferred - see README TODO).
type simTalos struct {
	amt *sim.AMT
}

func (s *simTalos) Shutdown(_ context.Context, _ string) error {
	s.amt.SetPower(false)
	return nil
}

// Full loop: cordon → drain → iSCSI gate (real truenas client + sim) →
// talos shutdown (fake, triggers logout + power-off) → wait BMC=off (real
// amtwsman client + sim). Asserts the recorded steps end with power off and a
// cleared storage gate.
func TestDrainShutdownFullLoopAgainstSims(t *testing.T) {
	const (
		nodeName = "node-1"
		nodeIQN  = "iqn.2005-03.org.open-iscsi:node-1"
		nodeAddr = "192.0.2.10"
		apiKey   = "1-fullloop-key"
	)

	tn := sim.NewTrueNAS(apiKey, sim.Session{Initiator: nodeIQN, InitiatorAddr: nodeAddr, Target: "iqn.2011-08.com.example:tank/k3s"})
	defer tn.Close()
	amt := sim.NewAMT(true /* on */, "Digest:sim")
	defer amt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Real TrueNAS client → sim, feeding the real iSCSI gate.
	tnClient, err := truenas.New(ctx, tn.Host(), "nightwatch", apiKey, truenas.WithInsecureSkipVerify())
	if err != nil {
		t.Fatalf("connect truenas sim: %v", err)
	}
	defer tnClient.Close()
	gate := iscsi.Gate{List: tnClient.SessionTable, Poll: 50 * time.Millisecond}

	// Real AMT client → sim, satisfying PowerController.
	amtClient := amtwsman.New(amt.Endpoint(), "admin", "secret")

	// Fake k8s: a Ready node with one evictable pod the drain must remove.
	cs := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec:       corev1.PodSpec{NodeName: nodeName},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	cs.Fake.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{
			Name:    "pods/eviction",
			Kind:    "Eviction",
			Group:   "policy",
			Version: "v1",
		}},
	}}
	// Simulate the apiserver: an accepted eviction deletes the pod and detaches
	// its iSCSI volume, clearing the node's session from the target - the real
	// chain that lets the storage gate (which runs after drain) report clear.
	cs.PrependReactor("create", "pods/eviction", func(a clienttesting.Action) (bool, runtime.Object, error) {
		e := a.(clienttesting.CreateAction).GetObject().(*policyv1.Eviction)
		_ = cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, e.Namespace, e.Name)
		tn.RemoveSessionByAddr(nodeAddr)
		return true, nil, nil
	})
	nodes := k8s.New(cs)

	deps := lifecycle.DrainShutdownDeps{
		Nodes:   nodes,
		Talos:   &simTalos{amt: amt},
		Power:   amtClient,
		Storage: lifecycle.StorageGateFunc(func(ctx context.Context, timeout time.Duration) error { return gate.WaitClear(ctx, nodeAddr, timeout) }),
	}
	opts := lifecycle.DrainShutdownOptions{
		TalosEndpoint:   nodeAddr,
		DrainTimeout:    10 * time.Second,
		StorageTimeout:  10 * time.Second,
		PowerOffTimeout: 10 * time.Second,
		PollInterval:    50 * time.Millisecond,
	}

	steps, err := lifecycle.DrainShutdown(ctx, nodeName, deps, opts)
	if err != nil {
		t.Fatalf("DrainShutdown: %v\nsteps: %+v", err, steps)
	}

	for _, s := range steps {
		if !s.Succeeded {
			t.Fatalf("step %q failed: %s", s.Name, s.Message)
		}
	}
	if last := steps[len(steps)-1]; last.Name != "wait-power-off" {
		t.Fatalf("final step = %q, want wait-power-off", last.Name)
	}
	if amt.IsOn() {
		t.Fatal("sim still on after full drain-shutdown loop")
	}
	if table, _ := tnClient.SessionTable(ctx); iscsi.SessionPresent(table, nodeAddr) {
		t.Fatalf("storage gate did not clear; session still present:\n%s", table)
	}
}
