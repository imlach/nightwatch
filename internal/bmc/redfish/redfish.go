// Package redfish is a Nightwatch BMC driver for Redfish-compatible BMCs.
// Power state is a GET of the single ComputerSystem;
// power actions POST ComputerSystem.Reset. Auth is HTTP Basic over HTTPS with
// a self-signed cert, so this adapter pins InsecureSkipVerify to the BMC host
// rather than touching the process-wide TLS config.
package redfish

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
)

const (
	// Single system member on iDRAC9; the embedded ID is firmware-fixed.
	systemPath = "/redfish/v1/Systems/System.Embedded.1"
	resetPath  = systemPath + "/Actions/ComputerSystem.Reset"

	// Redfish ResetType values (DMTF), mapped from bmc.Adapter ops below.
	resetOn               = "On"
	resetGracefulShutdown = "GracefulShutdown"
	resetForceOff         = "ForceOff"
	resetForceRestart     = "ForceRestart"
)

// Register both the canonical "redfish" type and the "idrac" alias so either
// bmc.type in the inventory resolves to this driver.
func init() {
	factory := func(host, username, password string) bmc.Adapter { return New(host, username, password) }
	bmc.Register("redfish", factory)
	bmc.Register("idrac", factory)
}

type Client struct {
	BaseURL    string // scheme+host, e.g. https://192.0.2.1
	Username   string
	Password   string
	HTTPClient *http.Client
}

// sharedTransport is reused by every Redfish adapter. The operator rebuilds a
// per-node adapter on every reconcile and never closes it (bmc.Adapter has no
// Close), so a per-adapter http.Transport would leak its idle-connection pool -
// each pooled keep-alive conn holds read/write goroutines, and with the default
// IdleConnTimeout of 0 they never expire. One shared, bounded transport keeps
// the pool finite and lets connections be reused across reconciles. TLS
// verification is skipped because iDRAC ships a self-signed cert - scoped to this
// transport only, never the process-wide default.
var sharedTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed iDRAC cert, scoped to this transport
	MaxIdleConnsPerHost: 2,
	IdleConnTimeout:     90 * time.Second,
}

// New builds a Redfish client. host may be a bare IP/host, host:port, or a full
// URL; it is normalized to an https base. The HTTP client skips TLS verification
// because iDRAC ships a self-signed cert - scoped to the shared transport only.
func New(host, username, password string) *Client {
	return &Client{
		BaseURL:  normalizeBaseURL(host),
		Username: username,
		Password: password,
		HTTPClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: sharedTransport,
		},
	}
}

// systemResponse is the subset of the ComputerSystem resource we read.
type systemResponse struct {
	PowerState string `json:"PowerState"`
}

// GetPowerState reads PowerState from the ComputerSystem resource ("On"/"Off").
func (c *Client) GetPowerState(ctx context.Context) bmc.Result {
	raw, status, err := c.do(ctx, http.MethodGet, systemPath, nil)
	if err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: err.Error(), Raw: raw}
	}
	if status < 200 || status > 299 {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: fmt.Sprintf("redfish GET system: HTTP %d", status), Raw: raw}
	}
	var sys systemResponse
	if err := json.Unmarshal([]byte(raw), &sys); err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: fmt.Sprintf("redfish decode system: %v", err), Raw: raw}
	}
	state := mapReadPowerState(sys.PowerState)
	if state == bmc.PowerUnknown {
		return bmc.Result{OK: false, PowerState: state, Error: fmt.Sprintf("redfish unexpected PowerState %q", sys.PowerState), Raw: raw}
	}
	return bmc.Result{OK: true, PowerState: state, Raw: raw}
}

func (c *Client) PowerOn(ctx context.Context) bmc.Result {
	return c.reset(ctx, resetOn)
}

func (c *Client) SoftOff(ctx context.Context) bmc.Result {
	return c.reset(ctx, resetGracefulShutdown)
}

func (c *Client) HardOff(ctx context.Context) bmc.Result {
	return c.reset(ctx, resetForceOff)
}

func (c *Client) Reset(ctx context.Context) bmc.Result {
	return c.reset(ctx, resetForceRestart)
}

// reset POSTs ComputerSystem.Reset with the given ResetType. iDRAC answers a
// successful action with 204 No Content (some firmware returns 200); any non-2xx
// is an error.
func (c *Client) reset(ctx context.Context, resetType string) bmc.Result {
	body := fmt.Sprintf(`{"ResetType":%q}`, resetType)
	raw, status, err := c.do(ctx, http.MethodPost, resetPath, strings.NewReader(body))
	if err != nil {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: err.Error(), Raw: raw}
	}
	if status < 200 || status > 299 {
		return bmc.Result{OK: false, PowerState: bmc.PowerUnknown, Error: fmt.Sprintf("redfish reset %s: HTTP %d", resetType, status), Raw: raw}
	}
	return bmc.Result{OK: true, PowerState: intendedPowerState(resetType), Raw: raw}
}

// do issues an authenticated Redfish request and returns the body, status, and
// any transport error. Basic auth is set on every request - iDRAC gates
// everything under /Systems behind it.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (string, int, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return string(b), resp.StatusCode, nil
}

// normalizeBaseURL coerces host into a scheme+host base URL (no path). Bare
// hosts and host:port default to https; an explicit http:// is preserved.
func normalizeBaseURL(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		if parsed, err := url.Parse(host); err == nil {
			return parsed.Scheme + "://" + parsed.Host
		}
		return host
	}
	return "https://" + host
}

// mapReadPowerState maps a Redfish PowerState to on/off. iDRAC reports "On" or
// "Off"; anything else (e.g. "PoweringOn"/"Paused") is unknown.
func mapReadPowerState(state string) bmc.PowerState {
	switch state {
	case "On":
		return bmc.PowerOn
	case "Off":
		return bmc.PowerOff
	default:
		return bmc.PowerUnknown
	}
}

// intendedPowerState maps a ResetType to the power state the host is heading
// toward, for reporting a power action's intent.
func intendedPowerState(resetType string) bmc.PowerState {
	switch resetType {
	case resetOn, resetForceRestart:
		return bmc.PowerOn
	case resetGracefulShutdown, resetForceOff:
		return bmc.PowerOff
	default:
		return bmc.PowerUnknown
	}
}
