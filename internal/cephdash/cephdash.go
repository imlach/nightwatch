// Package cephdash talks to a Ceph cluster's Manager Dashboard REST API
// (mgr/dashboard, https://<mgr-host>/api/...) to read RBD image status for the
// Ceph storage-gate backend (plan risk R5, see internal/cephrbd). It plays the
// same role internal/truenas plays for the iSCSI gate: the network-reachable
// "ask the storage side directly" client, so the gate stays trustworthy even
// if the node itself is unreachable during a UPS load-shed.
//
// # Why the dashboard API, and what is NOT confirmed yet
//
// No cgo is allowed (rules out go-ceph/librados), which leaves either an HTTP
// API or shelling out to the rbd/ceph CLIs (see cephrbd.CommandLister for the
// latter). The mgr/dashboard module ships enabled-by-default on every Ceph
// cluster since Nautilus, needs no ceph.conf/keyring on the nightwatch host,
// and authenticates over plain HTTPS with a username/password issued by `ceph
// dashboard ac-user-create` - the lowest-friction option to wire from a
// separate operator host. The alternative mgr/restful module was also
// considered: it maps arbitrary mon/mgr commands to HTTP, but RBD watcher
// state is read from the image header object directly (what `rbd status`
// does), which is not one of the commands that module exposes - it would not
// get us watcher data any more directly than the dashboard does.
//
// What this draft has NOT verified against a live cluster: whether
// GET /api/block/image/<pool>/<image> actually surfaces live watcher
// addresses (vs. only static image metadata), and the exact path/query-escaping
// the dashboard expects for the "<pool>/<image>" spec. See the PR's open
// questions. Because of that uncertainty, WatcherTable deliberately returns
// each endpoint's raw response body untouched, never a parsed struct - like
// truenas.SessionTable, so the gate's substring match on the client address
// keeps working even if the exact field name/shape is wrong or renamed
// between Ceph releases, and swapping the endpoint later (or pointing this at
// a small shim) needs no change to cephrbd.Gate.
package cephdash

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// config holds dial-time options (mirrors internal/truenas's Option pattern).
type config struct {
	tlsConfig *tls.Config
	insecure  bool
}

// Option configures the Ceph dashboard connection.
type Option func(*config)

// WithInsecureSkipVerify disables TLS certificate verification - needed when
// the dashboard presents a self-signed cert (a common default) reached by IP.
func WithInsecureSkipVerify() Option { return func(c *config) { c.insecure = true } }

// WithTLSConfig overrides the TLS config used for requests (e.g. to trust an
// internal CA). Takes precedence over WithInsecureSkipVerify.
func WithTLSConfig(t *tls.Config) Option { return func(c *config) { c.tlsConfig = t } }

// Client is an authenticated session against a Ceph mgr dashboard.
type Client struct {
	http  *http.Client
	base  string
	token string
}

// New logs into the dashboard at host (a host, host:port, or a full
// "http(s)://..." base for tests) and returns a Client holding the session's
// bearer token. host without an explicit scheme is dialed over https, since a
// dashboard's API key/password should never cross the wire in the clear.
func New(ctx context.Context, host, username, password string, opts ...Option) (*Client, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	tlsConf := cfg.tlsConfig
	if tlsConf == nil {
		tlsConf = &tls.Config{InsecureSkipVerify: cfg.insecure} //nolint:gosec // opt-in, internal target
	}
	c := &Client{
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}},
		base: baseURL(host),
	}
	if err := c.login(ctx, username, password); err != nil {
		return nil, err
	}
	return c, nil
}

func baseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + host
}

// login authenticates with the dashboard's session-token endpoint. The
// dashboard also supports API-key-per-user auth on newer releases;
// username+password is what every supported Ceph release accepts, so it's
// the baseline this draft wires - see the PR's open questions for whether
// Ross's cluster would rather use a longer-lived API key instead.
func (c *Client) login(ctx context.Context, username, password string) error {
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return fmt.Errorf("cephdash: encode login request: %w", err)
	}
	raw, err := c.request(ctx, http.MethodPost, "/api/auth", body, false)
	if err != nil {
		return fmt.Errorf("cephdash: login: %w", err)
	}
	var res struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("cephdash: decode login response: %w", err)
	}
	if res.Token == "" {
		return fmt.Errorf("cephdash: login: empty token in response")
	}
	c.token = res.Token
	return nil
}

// WatcherTable fetches each image's status from the dashboard and
// concatenates the raw response bodies into one text blob for
// cephrbd.WatcherPresent to substring-match against - exactly as
// truenas.SessionTable does for iSCSI sessions; see the package doc for why
// raw text (not parsed fields) is the deliberate choice here. images are
// "pool/image" specs (see operate.CephEnv / NIGHTWATCH_CEPH_IMAGES).
func (c *Client) WatcherTable(ctx context.Context, images []string) (string, error) {
	if len(images) == 0 {
		return "", fmt.Errorf("cephdash: no images configured to watch")
	}
	var out bytes.Buffer
	for _, img := range images {
		raw, err := c.request(ctx, http.MethodGet, "/api/block/image/"+url.PathEscape(img), nil, true)
		if err != nil {
			return "", fmt.Errorf("cephdash: image %s: %w", img, err)
		}
		out.Write(raw)
		out.WriteByte('\n')
	}
	return out.String(), nil
}

// Close best-effort logs the session out. Errors are not actionable by the
// caller (the storage gate has already finished its wait by the time Close
// runs), matching truenas.Client's Close contract.
func (c *Client) Close() error {
	if c.token == "" {
		return nil
	}
	_, err := c.request(context.Background(), http.MethodPost, "/api/auth/logout", nil, true)
	return err
}

// request issues one HTTP call and returns the raw response body. A non-2xx
// status is surfaced as an error carrying the body text (dashboard error
// responses are small JSON objects with a "detail" field) rather than being
// silently swallowed - same "don't mistake failure to read for success" rule
// the gate itself follows.
func (c *Client) request(ctx context.Context, method, path string, body []byte, authed bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.ceph.api.v1.0+json")
	if authed {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}
