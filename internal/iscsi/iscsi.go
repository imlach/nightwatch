// Package iscsi implements the pre-shutdown data-safety gate (plan risk R5): a
// node must have detached all of its iSCSI sessions before it loses power, or
// the storage target risks a half-open session / unclean teardown. This matters
// doubly during a UPS outage load-shed, where the storage target is the
// very thing being protected.
package iscsi

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SessionLister returns the current iSCSI session table as text, queried from
// the target side. The exact command is environment-specific
// (TrueNAS CTL `ctladm islist`, `iscsiadm -m session -P 1`, or the TrueNAS API);
// it is injected so the gate logic stays testable and storage-agnostic.
type SessionLister func(ctx context.Context) (string, error)

// Gate waits for a node's iSCSI sessions to clear before its power is pulled.
type Gate struct {
	List SessionLister
	// Poll is the interval between session-table checks (default 3s).
	Poll time.Duration
}

// WaitClear blocks until initiatorIQN no longer appears in the session table,
// or timeout elapses. A lister error is surfaced immediately rather than
// retried - failing to *read* sessions must not be mistaken for "clear".
func (g Gate) WaitClear(ctx context.Context, initiatorIQN string, timeout time.Duration) error {
	if g.List == nil {
		return fmt.Errorf("iscsi gate: no SessionLister configured")
	}
	if strings.TrimSpace(initiatorIQN) == "" {
		return fmt.Errorf("iscsi gate: empty initiator IQN")
	}
	poll := g.Poll
	if poll <= 0 {
		poll = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		table, err := g.List(ctx)
		if err != nil {
			return fmt.Errorf("iscsi gate: list sessions: %w", err)
		}
		if !SessionPresent(table, initiatorIQN) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("iscsi gate: sessions for %s still present after %s: %w", initiatorIQN, timeout, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// SessionPresent reports whether the initiator IQN appears in a session table.
// Match is case-insensitive - IQNs are defined case-insensitive and targets
// vary in how they echo them back.
func SessionPresent(table, initiatorIQN string) bool {
	return strings.Contains(strings.ToLower(table), strings.ToLower(strings.TrimSpace(initiatorIQN)))
}

// CommandLister builds a SessionLister that runs an external command (the
// argv to query the target's session table) and returns its combined output.
// Wire this to the confirmed storage-target command once an integration pins it down.
func CommandLister(name string, args ...string) SessionLister {
	return func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
}
