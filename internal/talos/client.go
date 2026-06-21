// Package talos drives a node's Talos machine API for the lifecycle: a graceful
// OS-level Shutdown (the primary shutdown path, before any BMC fallback) and a
// Reachable check used on wake. It speaks the Talos Go API directly - no
// talosctl subprocess in the runtime.
package talos

import (
	"context"
	"fmt"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
)

// machineClient is the subset of the Talos machinery client Nightwatch uses,
// extracted so the adapter is unit-testable with a fake.
type machineClient interface {
	Shutdown(ctx context.Context, opts ...client.ShutdownOption) error
	Version(ctx context.Context, opts ...grpc.CallOption) (*machineapi.VersionResponse, error)
	Close() error
}

// The real machinery client must satisfy the subset.
var _ machineClient = (*client.Client)(nil)

// Client wraps the Talos machine API.
type Client struct {
	mc machineClient
}

// New builds a Talos client from a talosconfig file. Endpoints (the control
// planes apid connects through) come from the config unless overridden; per-call
// node targeting selects the worker to act on.
func New(ctx context.Context, configPath string, endpoints ...string) (*Client, error) {
	opts := []client.OptionFunc{client.WithConfigFromFile(configPath)}
	if len(endpoints) > 0 {
		opts = append(opts, client.WithEndpoints(endpoints...))
	}
	mc, err := client.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("talos client: %w", err)
	}
	return &Client{mc: mc}, nil
}

func newWithClient(mc machineClient) *Client { return &Client{mc: mc} }

// Shutdown gracefully powers the node off at the OS level (non-forced so Talos
// unmounts filesystems + logs out iSCSI sessions cleanly). node is the Talos
// node IP to target.
func (c *Client) Shutdown(ctx context.Context, node string) error {
	if err := c.mc.Shutdown(client.WithNode(ctx, node), client.WithShutdownForce(false)); err != nil {
		return fmt.Errorf("talos shutdown %s: %w", node, err)
	}
	return nil
}

// Reachable reports whether the node's Talos API answers a Version call.
func (c *Client) Reachable(ctx context.Context, node string) bool {
	_, err := c.mc.Version(client.WithNode(ctx, node))
	return err == nil
}

// Close releases the underlying connection.
func (c *Client) Close() error { return c.mc.Close() }
