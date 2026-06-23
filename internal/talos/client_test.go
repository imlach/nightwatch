package talos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"google.golang.org/grpc"
)

type fakeMC struct {
	shutdownErr    error
	versionErr     error
	shutdownCalled bool
}

func (f *fakeMC) Shutdown(context.Context, ...client.ShutdownOption) error {
	f.shutdownCalled = true
	return f.shutdownErr
}

func (f *fakeMC) Version(context.Context, ...grpc.CallOption) (*machineapi.VersionResponse, error) {
	return &machineapi.VersionResponse{}, f.versionErr
}

func (f *fakeMC) Close() error { return nil }

func TestShutdown(t *testing.T) {
	mc := &fakeMC{}
	if err := newWithClient(mc).Shutdown(context.Background(), "192.0.2.10"); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	if !mc.shutdownCalled {
		t.Fatal("machinery Shutdown not called")
	}
	if err := newWithClient(&fakeMC{shutdownErr: errors.New("apid down")}).Shutdown(context.Background(), "n"); err == nil {
		t.Fatal("want error when machinery shutdown fails")
	}
}

func TestReachable(t *testing.T) {
	if !newWithClient(&fakeMC{}).Reachable(context.Background(), "n") {
		t.Fatal("want reachable when Version succeeds")
	}
	if newWithClient(&fakeMC{versionErr: errors.New("unreachable")}).Reachable(context.Background(), "n") {
		t.Fatal("want unreachable when Version errors")
	}
}

func TestNewBadConfig(t *testing.T) {
	if _, err := New(context.Background(), "/nonexistent/talosconfig"); err == nil {
		t.Fatal("want error for a missing talosconfig")
	}
}

func TestLoadConfigReadOnlyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "talosconfig")
	if err := os.WriteFile(path, []byte(`
context: default
contexts:
  default:
    endpoints:
      - 192.0.2.1
    ca: Q0E=
    crt: Q1JU
    key: S0VZ
`), 0o400); err != nil {
		t.Fatalf("write talosconfig: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig(read-only path) = %v", err)
	}
	if cfg.Context != "default" {
		t.Fatalf("context = %q, want default", cfg.Context)
	}
}
