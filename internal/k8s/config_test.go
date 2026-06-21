package k8s

import "testing"

func TestNewFromKubeconfigBadPath(t *testing.T) {
	// An explicit, missing kubeconfig must error (and not silently fall back to
	// a real config on the dev/CI host) - ExplicitPath overrides default rules.
	if _, err := NewFromKubeconfig("/nonexistent/nightwatch-kubeconfig"); err == nil {
		t.Fatal("NewFromKubeconfig with a missing path = nil error, want error")
	}
}
