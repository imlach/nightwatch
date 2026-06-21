//go:build integration

package sim_test

import (
	"context"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/iscsi"
	"github.com/imlach/nightwatch/internal/sim"
	"github.com/imlach/nightwatch/internal/truenas"
)

const (
	itAPIKey   = "1-integration-key"
	itNodeIQN  = "iqn.2005-03.org.open-iscsi:node-1"
	itNodeAddr = "192.0.2.10"
)

// Drives the real truenas.Client wsConn/JSON-RPC framing against the sim over a
// real wss:// handshake: login succeeds, the seeded session is visible to the
// gate, and clearing it makes the gate report clear.
func TestTrueNASClientAgainstSim(t *testing.T) {
	s := sim.NewTrueNAS(itAPIKey, sim.Session{
		Initiator:     itNodeIQN,
		InitiatorAddr: itNodeAddr,
		Target:        "iqn.2011-08.com.example:tank/k3s",
	})
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := truenas.New(ctx, s.Host(), "nightwatch", itAPIKey, truenas.WithInsecureSkipVerify())
	if err != nil {
		t.Fatalf("truenas.New (login over real wss): %v", err)
	}
	defer client.Close()

	table, err := client.SessionTable(ctx)
	if err != nil {
		t.Fatalf("SessionTable: %v", err)
	}
	if !iscsi.SessionPresent(table, itNodeAddr) {
		t.Fatalf("seeded session not visible to gate:\n%s", table)
	}

	// Model the node powering down: its session disappears from the target.
	s.RemoveSessionByAddr(itNodeAddr)

	gate := iscsi.Gate{List: client.SessionTable, Poll: 50 * time.Millisecond}
	if err := gate.WaitClear(ctx, itNodeAddr, 5*time.Second); err != nil {
		t.Fatalf("gate.WaitClear after session removed: %v", err)
	}
}

// A wrong API key must fail login (response_type != SUCCESS), proving the sim's
// auth path and the client's rejection both work over the wire.
func TestTrueNASClientLoginRejected(t *testing.T) {
	s := sim.NewTrueNAS(itAPIKey)
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := truenas.New(ctx, s.Host(), "nightwatch", "wrong-key", truenas.WithInsecureSkipVerify()); err == nil {
		t.Fatal("truenas.New = nil error, want login rejection for wrong API key")
	}
}
