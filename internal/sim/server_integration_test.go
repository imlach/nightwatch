//go:build integration

package sim_test

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/bmc/amtwsman"
	"github.com/imlach/nightwatch/internal/iscsi"
	"github.com/imlach/nightwatch/internal/sim"
	"github.com/imlach/nightwatch/internal/truenas"
)

// Exercises the fixed-port server wrappers (the container path's code) on
// loopback against the real clients, separate from the httptest wrappers.

func TestTrueNASServerWrapper(t *testing.T) {
	const key = "1-server-key"
	s := sim.NewTrueNASServer(key, sim.Session{Initiator: "iqn:x", InitiatorAddr: "192.0.2.10"})
	srv := s.HTTPServer("127.0.0.1:0")
	ln := mustListen(t)
	go func() {
		cert, err := tls.X509KeyPair(s.TLSCertPEM(), s.TLSKeyPEM())
		if err != nil {
			return
		}
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		_ = srv.ServeTLS(ln, "", "")
	}()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := truenas.New(ctx, ln.Addr().String(), "nightwatch", key, truenas.WithInsecureSkipVerify())
	if err != nil {
		t.Fatalf("truenas.New against fixed-port server: %v", err)
	}
	defer c.Close()
	table, err := c.SessionTable(ctx)
	if err != nil {
		t.Fatalf("SessionTable: %v", err)
	}
	if !iscsi.SessionPresent(table, "192.0.2.10") {
		t.Fatalf("seeded session not visible:\n%s", table)
	}
}

func TestAMTServerWrapper(t *testing.T) {
	s := sim.NewAMTServer(true, "Digest:sim")
	srv := s.HTTPServer("127.0.0.1:0")
	ln := mustListen(t)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c := amtwsman.New("http://"+ln.Addr().String(), "admin", "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if res := c.GetPowerState(ctx); !res.OK || res.PowerState != bmc.PowerOn {
		t.Fatalf("GetPowerState = %+v, want ok on", res)
	}
	if res := c.SoftOff(ctx); !res.OK {
		t.Fatalf("SoftOff = %+v", res)
	}
	if s.IsOn() {
		t.Fatal("sim still on after SoftOff")
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}
