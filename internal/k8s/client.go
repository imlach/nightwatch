// Package k8s wraps the subset of the Kubernetes API Nightwatch needs to gate a
// node in and out of service: readiness / GPU-registration checks, cordon, and a
// drain backed by kubectl's upstream drain helper.
package k8s

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	kubedrain "k8s.io/kubectl/pkg/drain"
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
	PollInterval       time.Duration // retry delay after PDB/eviction errors; default 2s
	Timeout            time.Duration // default 5m
}

// Drain evicts every evictable pod on the node via kubectl's drain helper, so
// PDBs, DaemonSet pods, mirror/static pods, unmanaged pods, emptyDir pods, and
// deletion waiting follow upstream kubectl behavior. Nightwatch keeps terminal
// or already-terminating pods out of the drain set so a stale completed pod does
// not block the iSCSI gate + power-off path.
func (c *Client) Drain(ctx context.Context, node string, opts DrainOptions) error {
	poll := opts.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	grace := -1 // kubectl/pkg/drain uses negative to preserve each pod's grace period.
	if opts.GracePeriodSeconds != nil {
		grace = int(*opts.GracePeriodSeconds)
	}

	drainer := &kubedrain.Helper{
		Ctx:                             ctx,
		Client:                          c.cs,
		Force:                           true,
		GracePeriodSeconds:              grace,
		IgnoreAllDaemonSets:             true,
		Timeout:                         timeout,
		DeleteEmptyDirData:              true,
		EvictErrorRetryDelay:            poll,
		Out:                             io.Discard,
		ErrOut:                          io.Discard,
		AdditionalFilters:               []kubedrain.PodFilter{sameNodeFilter(node), skipTerminalOrTerminatingPod},
		SkipWaitForDeleteTimeoutSeconds: 0,
	}
	if err := kubedrain.RunNodeDrain(drainer, node); err != nil {
		return fmt.Errorf("drain %s: %w", node, err)
	}
	return nil
}

func sameNodeFilter(node string) kubedrain.PodFilter {
	return func(pod corev1.Pod) kubedrain.PodDeleteStatus {
		// Real API servers apply the field selector in kubectl's pod list. Fake
		// clients do not, so keep tests honest with the same check here.
		if pod.Spec.NodeName != node {
			return kubedrain.MakePodDeleteStatusSkip()
		}
		return kubedrain.MakePodDeleteStatusOkay()
	}
}

func skipTerminalOrTerminatingPod(pod corev1.Pod) kubedrain.PodDeleteStatus {
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return kubedrain.MakePodDeleteStatusSkip()
	}
	return kubedrain.MakePodDeleteStatusOkay()
}
