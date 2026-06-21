package bmc_test

import (
	"testing"

	"github.com/imlach/nightwatch/internal/bmc"
	_ "github.com/imlach/nightwatch/internal/bmc/amtwsman" // registers the amt driver
)

func TestNewRegistered(t *testing.T) {
	adapter, err := bmc.New("amt", "h", "u", "p")
	if err != nil {
		t.Fatalf("New(amt) error = %v, want nil", err)
	}
	if adapter == nil {
		t.Fatal("New(amt) adapter = nil, want non-nil")
	}
}

func TestNewUnsupported(t *testing.T) {
	for _, typ := range []string{"idrac", "bogus"} {
		adapter, err := bmc.New(typ, "h", "u", "p")
		if err == nil {
			t.Fatalf("New(%q) error = nil, want non-nil", typ)
		}
		if adapter != nil {
			t.Fatalf("New(%q) adapter = %v, want nil", typ, adapter)
		}
	}
}

func TestRegisterPanicsOnDuplicate(t *testing.T) {
	f := func(host, username, password string) bmc.Adapter { return nil }
	bmc.Register("dup-test", f)
	defer func() {
		if recover() == nil {
			t.Fatal("Register duplicate did not panic")
		}
	}()
	bmc.Register("dup-test", f)
}

func TestRegisterPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register nil did not panic")
		}
	}()
	bmc.Register("nil-test", nil)
}
