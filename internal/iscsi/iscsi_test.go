package iscsi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const wkr01IQN = "iqn.2005-03.org.open-iscsi:node-1"

func TestSessionPresent(t *testing.T) {
	table := "Target iqn.2011-08.com.example:tank/k3s\n  Initiator IQN.2005-03.ORG.OPEN-ISCSI:NODE-1 (192.0.2.10)\n"
	if !SessionPresent(table, wkr01IQN) {
		t.Fatal("expected case-insensitive match for node-1 IQN")
	}
	if SessionPresent(table, "iqn.2005-03.org.open-iscsi:node-2") {
		t.Fatal("did not expect a match for node-2 IQN")
	}
}

func TestWaitClearAlreadyClear(t *testing.T) {
	g := Gate{List: func(context.Context) (string, error) { return "no sessions here", nil }, Poll: time.Millisecond}
	if err := g.WaitClear(context.Background(), wkr01IQN, time.Second); err != nil {
		t.Fatalf("WaitClear = %v, want nil when IQN absent", err)
	}
}

func TestWaitClearAfterDetach(t *testing.T) {
	calls := 0
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "Initiator " + wkr01IQN + " (192.0.2.10)", nil
		}
		return "sessions drained", nil
	}}
	if err := g.WaitClear(context.Background(), wkr01IQN, time.Second); err != nil {
		t.Fatalf("WaitClear = %v, want nil once sessions drain", err)
	}
	if calls < 3 {
		t.Fatalf("polled %d times, want >=3", calls)
	}
}

func TestWaitClearTimeout(t *testing.T) {
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) {
		return "Initiator " + wkr01IQN + " still attached", nil
	}}
	err := g.WaitClear(context.Background(), wkr01IQN, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still present") {
		t.Fatalf("WaitClear = %v, want timeout error while session persists", err)
	}
}

func TestWaitClearListerError(t *testing.T) {
	sentinel := errors.New("ctladm exploded")
	g := Gate{Poll: time.Millisecond, List: func(context.Context) (string, error) { return "", sentinel }}
	err := g.WaitClear(context.Background(), wkr01IQN, time.Second)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("WaitClear = %v, want wrapped lister error (must not be read as clear)", err)
	}
}

func TestWaitClearValidates(t *testing.T) {
	if err := (Gate{}).WaitClear(context.Background(), wkr01IQN, time.Second); err == nil {
		t.Fatal("want error when no SessionLister configured")
	}
	g := Gate{List: func(context.Context) (string, error) { return "", nil }}
	if err := g.WaitClear(context.Background(), "  ", time.Second); err == nil {
		t.Fatal("want error for empty initiator IQN")
	}
}
