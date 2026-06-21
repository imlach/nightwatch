package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewFromKubeconfig builds a Client from a kubeconfig file. An empty path uses
// the standard resolution (KUBECONFIG env, then ~/.kube/config); if that yields
// nothing it falls back to in-cluster config, so the same binary works mounted
// on a workstation or running as a Pod.
func NewFromKubeconfig(path string) (*Client, error) {
	cfg, err := restConfig(path)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube clientset: %w", err)
	}
	return New(cs), nil
}

func restConfig(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err == nil {
		return cfg, nil
	}
	if ic, icErr := rest.InClusterConfig(); icErr == nil {
		return ic, nil
	}
	return nil, fmt.Errorf("kubeconfig (path %q): %w", path, err)
}
