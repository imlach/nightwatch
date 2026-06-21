//go:build integration

package sim_test

import (
	"context"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/bmc/amtwsman"
	"github.com/imlach/nightwatch/internal/sim"
)

// Drives the real amtwsman.Client against the AMT sim over real HTTP + digest:
// GetPowerState reads the in-memory state via Enumerate→Pull, and the power
// actions flip it. The digest realm exercises the client's challenge/retry.
func TestAMTClientAgainstSim(t *testing.T) {
	s := sim.NewAMT(true /* on */, "Digest:sim")
	defer s.Close()

	client := amtwsman.New(s.Endpoint(), "admin", "secret")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if res := client.GetPowerState(ctx); !res.OK || res.PowerState != bmc.PowerOn {
		t.Fatalf("GetPowerState (initially on) = %+v, want ok on", res)
	}

	if res := client.SoftOff(ctx); !res.OK {
		t.Fatalf("SoftOff = %+v, want ok", res)
	}
	if s.IsOn() {
		t.Fatal("sim still on after SoftOff")
	}
	if res := client.GetPowerState(ctx); !res.OK || res.PowerState != bmc.PowerOff {
		t.Fatalf("GetPowerState (after SoftOff) = %+v, want ok off", res)
	}

	if res := client.PowerOn(ctx); !res.OK {
		t.Fatalf("PowerOn = %+v, want ok", res)
	}
	if !s.IsOn() {
		t.Fatal("sim still off after PowerOn")
	}

	if res := client.HardOff(ctx); !res.OK {
		t.Fatalf("HardOff = %+v, want ok", res)
	}
	if s.IsOn() {
		t.Fatal("sim still on after HardOff")
	}
	if res := client.GetPowerState(ctx); res.PowerState != bmc.PowerOff {
		t.Fatalf("GetPowerState (after HardOff) = %+v, want off", res)
	}
}

// Without a digest realm the sim accepts unauthenticated requests, so the client
// reads/changes state with no Authorization header - the no-auth wire path.
func TestAMTClientNoAuth(t *testing.T) {
	s := sim.NewAMT(false /* off */, "")
	defer s.Close()

	client := amtwsman.New(s.Endpoint(), "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if res := client.GetPowerState(ctx); !res.OK || res.PowerState != bmc.PowerOff {
		t.Fatalf("GetPowerState (initially off) = %+v, want ok off", res)
	}
	if res := client.PowerOn(ctx); !res.OK || res.PowerState != bmc.PowerOn {
		t.Fatalf("PowerOn = %+v, want ok on-intent", res)
	}
	if !s.IsOn() {
		t.Fatal("sim still off after PowerOn")
	}
}
