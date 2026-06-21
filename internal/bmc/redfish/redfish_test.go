package redfish

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imlach/nightwatch/internal/bmc"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// TestGetPowerState drives GetPowerState against a canned ComputerSystem body
// and asserts the GET path, Basic auth header, and on/off/unknown mapping.
func TestGetPowerState(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantOK    bool
		wantState bmc.PowerState
	}{
		{name: "on from fixture", body: readFixture(t, "system_on.json"), wantOK: true, wantState: bmc.PowerOn},
		{name: "off", body: `{"PowerState":"Off"}`, wantOK: true, wantState: bmc.PowerOff},
		{name: "transient is unknown", body: `{"PowerState":"PoweringOn"}`, wantOK: false, wantState: bmc.PowerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatalf("method = %s, want GET", r.Method)
				}
				if r.URL.Path != systemPath {
					t.Fatalf("path = %s, want %s", r.URL.Path, systemPath)
				}
				assertBasicAuth(t, r, "root", "calvin")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			result := New(server.URL, "root", "calvin").GetPowerState(context.Background())
			if result.OK != tt.wantOK || result.PowerState != tt.wantState {
				t.Fatalf("GetPowerState() = %+v, want ok=%v state=%q", result, tt.wantOK, tt.wantState)
			}
		})
	}
}

// TestGetPowerStateNon2xx maps a non-2xx GET to an error Result.
func TestGetPowerStateNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()

	result := New(server.URL, "root", "calvin").GetPowerState(context.Background())
	if result.OK || result.PowerState != bmc.PowerUnknown || !strings.Contains(result.Error, "401") {
		t.Fatalf("GetPowerState() = %+v, want error on 401", result)
	}
}

// TestResetActions checks each bmc.Adapter op POSTs the right ResetType to the
// reset action path with Basic auth, and reports the intended power state.
func TestResetActions(t *testing.T) {
	tests := []struct {
		name          string
		call          func(*Client, context.Context) bmc.Result
		wantResetType string
		wantState     bmc.PowerState
	}{
		{name: "PowerOn", call: (*Client).PowerOn, wantResetType: "On", wantState: bmc.PowerOn},
		{name: "SoftOff", call: (*Client).SoftOff, wantResetType: "GracefulShutdown", wantState: bmc.PowerOff},
		{name: "HardOff", call: (*Client).HardOff, wantResetType: "ForceOff", wantState: bmc.PowerOff},
		{name: "Reset", call: (*Client).Reset, wantResetType: "ForceRestart", wantState: bmc.PowerOn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", r.Method)
				}
				if r.URL.Path != resetPath {
					t.Fatalf("path = %s, want %s", r.URL.Path, resetPath)
				}
				assertBasicAuth(t, r, "root", "calvin")
				body := readBody(t, r)
				want := `{"ResetType":"` + tt.wantResetType + `"}`
				if body != want {
					t.Fatalf("reset body = %s, want %s", body, want)
				}
				// iDRAC answers a successful action with 204 No Content.
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			result := tt.call(New(server.URL, "root", "calvin"), context.Background())
			if !result.OK || result.PowerState != tt.wantState || result.Error != "" {
				t.Fatalf("%s() = %+v, want ok state=%q", tt.name, result, tt.wantState)
			}
		})
	}
}

// TestResetNon2xx maps a non-2xx action response to an error Result.
func TestResetNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad reset"}`)
	}))
	defer server.Close()

	result := New(server.URL, "root", "calvin").PowerOn(context.Background())
	if result.OK || result.PowerState != bmc.PowerUnknown || !strings.Contains(result.Error, "400") {
		t.Fatalf("PowerOn() = %+v, want error on 400", result)
	}
}

func TestRegistered(t *testing.T) {
	for _, typ := range []string{"redfish", "idrac"} {
		adapter, err := bmc.New(typ, "192.0.2.3", "root", "calvin")
		if err != nil {
			t.Fatalf("bmc.New(%q) error = %v, want nil", typ, err)
		}
		if adapter == nil {
			t.Fatalf("bmc.New(%q) adapter = nil, want non-nil", typ)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{host: "192.0.2.3", want: "https://192.0.2.3"},
		{host: "192.0.2.3:443", want: "https://192.0.2.3:443"},
		{host: "https://192.0.2.3", want: "https://192.0.2.3"},
		{host: "https://192.0.2.3/redfish/v1/", want: "https://192.0.2.3"},
		{host: "http://192.0.2.3:8000", want: "http://192.0.2.3:8000"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := normalizeBaseURL(tt.host); got != tt.want {
				t.Fatalf("normalizeBaseURL(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func assertBasicAuth(t *testing.T, r *http.Request, wantUser, wantPass string) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	const prefix = "Basic "
	if !strings.HasPrefix(auth, prefix) {
		t.Fatalf("Authorization = %q, want Basic auth", auth)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, prefix))
	if err != nil {
		t.Fatalf("decode basic auth: %v", err)
	}
	if got := string(decoded); got != wantUser+":"+wantPass {
		t.Fatalf("basic auth = %q, want %q", got, wantUser+":"+wantPass)
	}
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
