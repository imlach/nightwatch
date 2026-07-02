package cephdash

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/imlach/nightwatch/internal/cephrbd"
)

const wkr01Addr = "203.0.113.10"

// fakeDashboard is a minimal stand-in for mgr/dashboard: it authenticates one
// username/password pair and serves canned per-image bodies keyed by the
// (unescaped) "pool/image" path suffix.
type fakeDashboard struct {
	mu                 sync.Mutex
	username, password string
	images             map[string]string // "pool/image" -> raw response body
	authHdrs           []string          // Authorization headers seen by /api/block/image calls
	loggedOut          bool
}

func newFakeDashboard(t *testing.T, username, password string) (*httptest.Server, *fakeDashboard) {
	t.Helper()
	f := &fakeDashboard{username: username, password: password, images: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var creds struct{ Username, Password string }
		if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if creds.Username != f.username || creds.Password != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"invalid credentials"}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"tok-` + f.username + `"}`))
	})
	mux.HandleFunc("/api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.loggedOut = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/block/image/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authHdrs = append(f.authHdrs, r.Header.Get("Authorization"))
		spec := strings.TrimPrefix(r.URL.Path, "/api/block/image/")
		body, ok := f.images[spec]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"image not found"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	})
	return httptest.NewServer(mux), f
}

func TestLoginSuccess(t *testing.T) {
	srv, _ := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	c, err := New(context.Background(), srv.URL, "nightwatch", "s3cret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.token == "" {
		t.Fatal("New() left token empty after a successful login")
	}
}

func TestLoginDenied(t *testing.T) {
	srv, _ := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	if _, err := New(context.Background(), srv.URL, "nightwatch", "wrong"); err == nil {
		t.Fatal("New() error = nil, want error on bad credentials")
	}
}

// The load-bearing safety property: the aggregated table must let the gate
// see a present client address across multiple watched images, and report
// clear once none of them mention it.
func TestWatcherTableAggregatesImages(t *testing.T) {
	srv, f := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	f.images["pool1/img1"] = `{"name":"img1","watchers":[]}`
	f.images["pool2/img2"] = `{"name":"img2","watchers":[{"address":"` + wkr01Addr + `:0/1","entity":"client.4110"}]}`

	c, err := New(context.Background(), srv.URL, "nightwatch", "s3cret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	table, err := c.WatcherTable(context.Background(), []string{"pool1/img1", "pool2/img2"})
	if err != nil {
		t.Fatalf("WatcherTable() error = %v", err)
	}
	if !cephrbd.WatcherPresent(table, wkr01Addr) {
		t.Fatalf("gate did not see present watcher in aggregated table:\n%s", table)
	}
	if !strings.Contains(table, "img1") || !strings.Contains(table, "img2") {
		t.Fatalf("aggregated table missing one of the two images:\n%s", table)
	}

	f.images["pool2/img2"] = `{"name":"img2","watchers":[]}`
	table, err = c.WatcherTable(context.Background(), []string{"pool1/img1", "pool2/img2"})
	if err != nil {
		t.Fatalf("WatcherTable() (clear) error = %v", err)
	}
	if cephrbd.WatcherPresent(table, wkr01Addr) {
		t.Fatalf("gate saw a watcher after it drained:\n%s", table)
	}

	if len(f.authHdrs) == 0 || f.authHdrs[0] != "Bearer tok-nightwatch" {
		t.Fatalf("WatcherTable() authHdrs = %v, want bearer token on every image request", f.authHdrs)
	}
}

func TestWatcherTableRequiresImages(t *testing.T) {
	srv, _ := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	c, err := New(context.Background(), srv.URL, "nightwatch", "s3cret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := c.WatcherTable(context.Background(), nil); err == nil {
		t.Fatal("WatcherTable() error = nil, want error for empty image set")
	}
}

// A dashboard error on one image must surface as an error, not be read as
// "no watchers" for that image - same rule truenas enforces on RPC errors.
func TestWatcherTablePropagatesHTTPError(t *testing.T) {
	srv, f := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	f.images["pool1/img1"] = `{"name":"img1","watchers":[]}`
	c, err := New(context.Background(), srv.URL, "nightwatch", "s3cret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := c.WatcherTable(context.Background(), []string{"pool1/img1", "pool-missing/nope"}); err == nil {
		t.Fatal("WatcherTable() error = nil, want error when an image lookup 404s")
	}
}

func TestClose(t *testing.T) {
	srv, f := newFakeDashboard(t, "nightwatch", "s3cret")
	defer srv.Close()
	c, err := New(context.Background(), srv.URL, "nightwatch", "s3cret")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	f.mu.Lock()
	loggedOut := f.loggedOut
	f.mu.Unlock()
	if !loggedOut {
		t.Fatal("Close() did not call the dashboard logout endpoint")
	}
}
