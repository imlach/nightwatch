package cephrbd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const wkr01Addr = "203.0.113.10"

func TestWatcherPresent(t *testing.T) {
	table := `{"watchers":[{"address":"203.0.113.10:0/1234567890","client":"client.4110"}]}` + "\n"
	if !WatcherPresent(table, wkr01Addr) {
		t.Fatal("expected match for node-1 client address")
	}
	if WatcherPresent(table, "203.0.113.11") {
		t.Fatal("did not expect a match for node-2 client address")
	}
}

func TestWatcherPresentCaseInsensitive(t *testing.T) {
	table := "WATCHER=203.0.113.10:0/42 CLIENT.4110"
	if !WatcherPresent(table, wkr01Addr) {
		t.Fatal("expected case-insensitive match")
	}
}

func TestWaitDetachedAlreadyClear(t *testing.T) {
	g := Gate{List: func(context.Context) (string, error) { return "no watchers here", nil }, Poll: time.Millisecond}
	if err := g.WaitDetached(context.Background(), wkr01Addr, time.Second); err != nil {
		t.Fatalf("WaitDetached = %v, want nil when address absent", err)
	}
}

func TestWaitDetachedAfterDetach(t *testing.T) {
	calls := 0
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "watcher=" + wkr01Addr + ":0/1 client.4110", nil
		}
		return "no watchers", nil
	}}
	if err := g.WaitDetached(context.Background(), wkr01Addr, time.Second); err != nil {
		t.Fatalf("WaitDetached = %v, want nil once watcher drains", err)
	}
	if calls < 3 {
		t.Fatalf("polled %d times, want >=3", calls)
	}
}

func TestWaitDetachedTimeout(t *testing.T) {
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) {
		return "watcher=" + wkr01Addr + ":0/1 client.4110", nil
	}}
	err := g.WaitDetached(context.Background(), wkr01Addr, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("WaitDetached = %v, want timeout error while watcher persists", err)
	}
}

func TestWaitDetachedListerError(t *testing.T) {
	sentinel := errors.New("dashboard unreachable")
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) { return "", sentinel }}
	err := g.WaitDetached(context.Background(), wkr01Addr, time.Second)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("WaitDetached = %v, want wrapped lister error (must not be read as detached)", err)
	}
}

func TestWaitDetachedValidates(t *testing.T) {
	if err := (Gate{}).WaitDetached(context.Background(), wkr01Addr, time.Second); err == nil {
		t.Fatal("want error when no WatcherLister configured")
	}
	g := Gate{List: func(context.Context) (string, error) { return "", nil }}
	if err := g.WaitDetached(context.Background(), "  ", time.Second); err == nil {
		t.Fatal("want error for empty ceph client address")
	}
}
