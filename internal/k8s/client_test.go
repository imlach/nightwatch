package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func node(name string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}}},
	}
}

func gpuNode(name string) *corev1.Node {
	n := node(name, corev1.ConditionTrue)
	n.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	return n
}

func pod(ns, name, onNode string, opts ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{NodeName: onNode},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withEvictionSupport(cs *fake.Clientset) *fake.Clientset {
	cs.Fake.Resources = []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{{
			Name:    "pods/eviction",
			Kind:    "Eviction",
			Group:   "policy",
			Version: "v1",
		}},
	}}
	return cs
}

func daemonSet(ns, name string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func daemonSetPod(ns, name, onNode, dsName string) *corev1.Pod {
	controller := true
	return pod(ns, name, onNode, func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.SchemeGroupVersion.String(),
			Kind:       "DaemonSet",
			Name:       dsName,
			Controller: &controller,
		}}
	})
}

func TestIsNodeReady(t *testing.T) {
	c := New(fake.NewClientset(node("ready", corev1.ConditionTrue), node("down", corev1.ConditionFalse)))
	if ok, err := c.IsNodeReady(context.Background(), "ready"); err != nil || !ok {
		t.Fatalf("ready = %v, %v; want true", ok, err)
	}
	if ok, _ := c.IsNodeReady(context.Background(), "down"); ok {
		t.Fatal("down node should not be ready")
	}
	if _, err := c.IsNodeReady(context.Background(), "ghost"); err == nil {
		t.Fatal("missing node should error")
	}
}

func TestNodeHasGPUCapacity(t *testing.T) {
	c := New(fake.NewClientset(gpuNode("gpu"), node("cpu", corev1.ConditionTrue)))
	if ok, err := c.NodeHasGPUCapacity(context.Background(), "gpu"); err != nil || !ok {
		t.Fatalf("gpu = %v, %v; want true", ok, err)
	}
	if ok, _ := c.NodeHasGPUCapacity(context.Background(), "cpu"); ok {
		t.Fatal("cpu node should report no GPU capacity")
	}
}

func TestCordonUncordon(t *testing.T) {
	cs := fake.NewClientset(node("n1", corev1.ConditionTrue))
	c := New(cs)
	if ok, err := c.IsNodeSchedulable(context.Background(), "n1"); err != nil || !ok {
		t.Fatalf("initial schedulable = %v, %v; want true", ok, err)
	}
	if err := c.Cordon(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{}); !n.Spec.Unschedulable {
		t.Fatal("want unschedulable after Cordon")
	}
	if ok, err := c.IsNodeSchedulable(context.Background(), "n1"); err != nil || ok {
		t.Fatalf("cordoned schedulable = %v, %v; want false", ok, err)
	}
	if err := c.Uncordon(context.Background(), "n1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := cs.CoreV1().Nodes().Get(context.Background(), "n1", metav1.GetOptions{}); n.Spec.Unschedulable {
		t.Fatal("want schedulable after Uncordon")
	}
}

func TestSkipTerminalOrTerminatingPod(t *testing.T) {
	terminating := pod("ns", "term", "n1", func(p *corev1.Pod) {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	})
	succeeded := pod("ns", "done", "n1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded })
	failed := pod("ns", "failed", "n1", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed })
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"normal", pod("ns", "app", "n1"), true},
		{"terminating", terminating, false},
		{"succeeded", succeeded, false},
		{"failed", failed, false},
	}
	for _, tt := range tests {
		if got := skipTerminalOrTerminatingPod(*tt.pod).Delete; got != tt.want {
			t.Errorf("skipTerminalOrTerminatingPod(%s).Delete = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDrainEvictsAndWaits(t *testing.T) {
	cs := withEvictionSupport(fake.NewClientset(
		pod("inference", "app1", "node-1"),
		daemonSet("kube-system", "cilium"),
		daemonSetPod("kube-system", "cilium-pod", "node-1", "cilium"),
		pod("inference", "app2", "node-2"),
	))
	// Simulate the apiserver: an accepted eviction deletes the pod.
	cs.PrependReactor("create", "pods/eviction", func(a clienttesting.Action) (bool, runtime.Object, error) {
		e := a.(clienttesting.CreateAction).GetObject().(*policyv1.Eviction)
		_ = cs.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, e.Namespace, e.Name)
		return true, nil, nil
	})

	err := New(cs).Drain(context.Background(), "node-1", DrainOptions{PollInterval: time.Millisecond, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("Drain = %v, want nil", err)
	}

	remaining, _ := cs.CoreV1().Pods(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	present := map[string]bool{}
	for _, p := range remaining.Items {
		present[p.Namespace+"/"+p.Name] = true
	}
	if present["inference/app1"] {
		t.Error("app1 (evictable, node-1) should be gone")
	}
	if !present["kube-system/cilium-pod"] {
		t.Error("daemonset pod must be left running")
	}
	if !present["inference/app2"] {
		t.Error("pod on another node must be untouched")
	}
}

func TestDrainTimeoutNamesRemainingPods(t *testing.T) {
	cs := withEvictionSupport(fake.NewClientset(pod("monitoring", "prometheus-1", "node-1")))
	cs.PrependReactor("create", "pods/eviction", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewTooManyRequests("pdb blocked", 1)
	})

	err := New(cs).Drain(context.Background(), "node-1", DrainOptions{PollInterval: time.Millisecond, Timeout: 5 * time.Millisecond})
	if err == nil {
		t.Fatal("Drain = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "monitoring") || !strings.Contains(err.Error(), "prometheus-1") {
		t.Fatalf("Drain error = %q, want remaining pod namespace and name", err)
	}
}
