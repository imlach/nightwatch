// Package cephrbd implements a second backend for the pre-shutdown
// data-safety gate (plan risk R5), for nodes whose storage is Ceph RBD rather
// than TrueNAS iSCSI: a node must have no live watcher on any of its mapped
// RBD images before it loses power, or the image's exclusive lock / client
// cache can be left inconsistent on the next map. It plays exactly the role
// internal/iscsi plays for the TrueNAS gate - see that package's doc for the
// shared rationale - and slots behind the same lifecycle.StorageGate
// interface (internal/lifecycle).
//
// One structural difference from iSCSI: TrueNAS exposes a single
// cluster-wide session table, but Ceph has no "list every RBD watcher in the
// cluster" call - watchers are scoped per image (`rbd status <pool>/<image>`
// contacts that image's header object directly). So the raw table Gate polls
// here is whatever text the caller's WatcherLister produces by aggregating
// per-image lookups over the image set it has been told to watch - see
// internal/cephdash for the HTTP-based lister this ships with. Gate itself
// stays agnostic to that aggregation and, like iscsi.Gate, is unit-testable
// without a live Ceph cluster.
package cephrbd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// WatcherLister returns the current RBD watcher table as text, aggregated
// across whatever set of images the caller is watching for this node. Exactly
// like iscsi.SessionLister, it is injected so the polling logic below stays
// storage-backend-agnostic and testable without a live cluster.
type WatcherLister func(ctx context.Context) (string, error)

// Gate waits for a node's Ceph client address to disappear from the watched
// images' watcher lists before its power is pulled.
type Gate struct {
	List WatcherLister
	// Poll is the interval between watcher-table checks (default 3s).
	Poll time.Duration
}

// WaitDetached blocks until clientAddr no longer appears in the watcher
// table, or timeout elapses. Mirrors iscsi.Gate.WaitClear exactly: a lister
// error is surfaced immediately rather than retried - failing to *read*
// watchers must never be mistaken for "detached".
func (g Gate) WaitDetached(ctx context.Context, clientAddr string, timeout time.Duration) error {
	if g.List == nil {
		return fmt.Errorf("cephrbd gate: no WatcherLister configured")
	}
	if strings.TrimSpace(clientAddr) == "" {
		return fmt.Errorf("cephrbd gate: empty ceph client address")
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
			return fmt.Errorf("cephrbd gate: list watchers: %w", err)
		}
		if !WatcherPresent(table, clientAddr) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cephrbd gate: watcher %s still present after %s: %w", clientAddr, timeout, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// WatcherPresent reports whether the Ceph client address appears in a watcher
// table. Match is a case-insensitive substring - the same tolerant contract
// as iscsi.SessionPresent. `rbd status` watcher entries look like
// "watcher=<addr>:0/<nonce> client.<id> cookie=...", and this deliberately
// does not try to parse that (or any JSON encoding of it) so the match stays
// correct no matter which endpoint/shape produced the raw text - see
// internal/cephdash's package doc for why that matters here in particular.
func WatcherPresent(table, clientAddr string) bool {
	return strings.Contains(strings.ToLower(table), strings.ToLower(strings.TrimSpace(clientAddr)))
}

// CommandLister builds a WatcherLister that shells out to the rbd CLI's
// `status` subcommand once per image and concatenates the raw JSON output -
// the exec-based alternative to internal/cephdash's HTTP client, for a
// nightwatch host that already carries a working ceph.conf + keyring (e.g. it
// runs alongside a mon/mgr) and would rather not stand up dashboard auth.
// Like iscsi.CommandLister, this is no-cgo and wired in only once the
// argv/access model is confirmed for the target environment.
func CommandLister(rbdPath string, images ...string) WatcherLister {
	if rbdPath == "" {
		rbdPath = "rbd"
	}
	return func(ctx context.Context) (string, error) {
		var out strings.Builder
		for _, img := range images {
			b, err := exec.CommandContext(ctx, rbdPath, "status", "--format", "json", img).CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%s status %s: %w: %s", rbdPath, img, err, strings.TrimSpace(string(b)))
			}
			out.Write(b)
			out.WriteByte('\n')
		}
		return out.String(), nil
	}
}
