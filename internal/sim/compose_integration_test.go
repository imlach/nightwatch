//go:build integration

package sim_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/bmc/amtwsman"
	"github.com/imlach/nightwatch/internal/truenas"
)

// These drive the real clients at externally-running sims (the docker-compose
// containers), pointed via env vars. They skip when unset, so the normal
// integration run (in-process sims above) is unaffected. Used by the `tests`
// service in docker-compose.test.yml.
//
//	NIGHTWATCH_SIM_TRUENAS   host:port of the TrueNAS sim (wss)
//	NIGHTWATCH_SIM_TRUENAS_KEY  expected API key (default test-key)
//	NIGHTWATCH_SIM_AMT       base URL of the AMT sim (http://host:port)

func TestComposeTrueNAS(t *testing.T) {
	host := os.Getenv("NIGHTWATCH_SIM_TRUENAS")
	if host == "" {
		t.Skip("NIGHTWATCH_SIM_TRUENAS unset; container path not running")
	}
	key := envOr("NIGHTWATCH_SIM_TRUENAS_KEY", "test-key")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := truenas.New(ctx, host, "nightwatch", key, truenas.WithInsecureSkipVerify())
	if err != nil {
		t.Fatalf("connect compose truenas sim %s: %v", host, err)
	}
	defer c.Close()
	if _, err := c.SessionTable(ctx); err != nil {
		t.Fatalf("SessionTable: %v", err)
	}
}

func TestComposeAMT(t *testing.T) {
	endpoint := os.Getenv("NIGHTWATCH_SIM_AMT")
	if endpoint == "" {
		t.Skip("NIGHTWATCH_SIM_AMT unset; container path not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := amtwsman.New(endpoint, "admin", "secret")
	if res := c.GetPowerState(ctx); !res.OK {
		t.Fatalf("GetPowerState against compose amt sim %s: %+v", endpoint, res)
	}
	if res := c.SoftOff(ctx); !res.OK || res.PowerState != bmc.PowerOff {
		t.Fatalf("SoftOff = %+v, want ok off", res)
	}
	if res := c.GetPowerState(ctx); res.PowerState != bmc.PowerOff {
		t.Fatalf("GetPowerState after SoftOff = %+v, want off", res)
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
