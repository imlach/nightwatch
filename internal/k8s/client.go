// Package k8s wraps the subset of the Kubernetes API Nightwatch needs to gate a
// node in and out of service: readiness / GPU-registration checks, cordon, and a
// real drain that waits for pods to actually terminate (not just for evictions
// to be issued). It is the in-code replacement for the hand-run `kubectl drain`
// used during the BMC spike.
package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Client is a thin wrapper over a Kubernetes clientset.
type Client struct {
	cs kubernetes.Interface
}

// New wraps an existing clientset (real or fake).
func New(cs kubernetes.Interface) *Client { return &Client{cs: cs} }

// IsNodeReady reports whether the node's Ready condition is True. A node that is
// rebooting/absent returns (false, nil) for a missing condition; only API errors
// surface as errors. Note: per the BMC spike, NodeReady can lag a fast reboot -
// the BMC power read is the authoritative up/off signal; this is a secondary gate.
func (c *Client) IsNodeReady(ctx context.Context, name string) (bool, error) {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue, nil
		}
	}
	return false, nil
}

// gpuResources are the allocatable keys that signal the NVIDIA + HAMi stack has
// re-registered after a wake. Any present and non-zero means the card is back.
var gpuResources = []corev1.ResourceName{"nvidia.com/gpu", "nvidia.com/gpumem"}

// NodeHasGPUCapacity reports whether the node advertises GPU allocatable - the
// wake-side gate that the device plugin has come back, not just the kubelet.
func (c *Client) NodeHasGPUCapacity(ctx context.Context, name string) (bool, error) {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	for _, r := range gpuResources {
		if q, ok := node.Status.Allocatable[r]; ok && !q.IsZero() {
			return true, nil
		}
	}
	return false, nil
}

// IsNodeSchedulable reports whether Kubernetes is allowed to place new work on
// the node. Ready alone is not enough after Nightwatch has cordoned before a
// shutdown.
func (c *Client) IsNodeSchedulable(ctx context.Context, name string) (bool, error) {
	node, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return !node.Spec.Unschedulable, nil
}

// Cordon marks the node unschedulable. Uncordon reverses it.
func (c *Client) Cordon(ctx context.Context, name string) error {
	return c.setUnschedulable(ctx, name, true)
}

func (c *Client) Uncordon(ctx context.Context, name string) error {
	return c.setUnschedulable(ctx, name, false)
}

func (c *Client) setUnschedulable(ctx context.Context, name string, v bool) error {
	patch := []byte(fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, v))
	_, err := c.cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	return err
}

// DrainOptions tunes a drain.
type DrainOptions struct {
	GracePeriodSeconds *int64        // nil → pod's terminationGracePeriodSeconds
	PollInterval       time.Duration // default 2s
	Timeout            time.Duration // default 5m
}

// Drain evicts every evictable pod on the node via the eviction API (so PDBs are
// respected) and blocks until none remain. DaemonSet-managed, mirror/static, and
// already-terminal pods are left alone. A PDB-blocked eviction (HTTP 429) is
// tolerated and retried on the next poll rather than failing the drain. The
// "wait until gone" is what makes this safe to chain before an iSCSI gate +
// power-off - `kubectl drain` returns on eviction, not on termination.
func (c *Client) Drain(ctx context.Context, node string, opts DrainOptions) error {
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		pods, err := c.evictablePods(ctx, node)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}
		for i := range pods {
			if err := c.evict(ctx, &pods[i], opts.GracePeriodSeconds); err != nil {
				// 429 → PDB currently blocks it; NotFound → already gone. Both retry.
				if !apierrors.IsTooManyRequests(err) && !apierrors.IsNotFound(err) {
					return fmt.Errorf("evict %s/%s: %w", pods[i].Namespace, pods[i].Name, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain %s: pods still present after %s: %w", node, timeout, ctx.Err())
		case <-time.After(poll):
		}
	}
}

func (c *Client) evictablePods(ctx context.Context, node string) ([]corev1.Pod, error) {
	list, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("spec.nodeName", node).String(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		// Defensive: the FieldSelector filters server-side in production, but
		// fake clients ignore it, so confirm the node here too.
		if p.Spec.NodeName != node {
			continue
		}
		if isEvictable(p) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (c *Client) evict(ctx context.Context, pod *corev1.Pod, grace *int64) error {
	return c.cs.PolicyV1().Evictions(pod.Namespace).Evict(ctx, &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: &metav1.DeleteOptions{GracePeriodSeconds: grace},
	})
}

// isEvictable reports whether drain should evict a pod: skip DaemonSet-managed,
// mirror (static) pods, and pods already terminal or terminating.
func isEvictable(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}
	if _, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]; ok {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return false
		}
	}
	return true
}
