package amtwsman

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imlach/nightwatch/internal/bmc"
)

func TestParsePowerState(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bmc.PowerState
	}{
		{name: "on", raw: `<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:PowerState>2</p:PowerState></s:Body></s:Envelope>`, want: bmc.PowerOn},
		{name: "soft off", raw: `<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:PowerState>8</p:PowerState></s:Body></s:Envelope>`, want: bmc.PowerOff},
		{name: "unknown", raw: `<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:PowerState>99</p:PowerState></s:Body></s:Envelope>`, want: bmc.PowerUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParsePowerState(tt.raw); got != tt.want {
				t.Fatalf("ParsePowerState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPowerOnPostsWSManRequest(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("SOAPAction"); got != powerChangeAction {
			t.Fatalf("SOAPAction = %q, want %q", got, powerChangeAction)
		}
		body := readBody(t, r)
		if !strings.Contains(body, "<p:PowerState>2</p:PowerState>") {
			t.Fatalf("request body missing power-on state: %s", body)
		}
		if !strings.Contains(body, "<a:To>"+server.URL+"/wsman</a:To>") {
			t.Fatalf("request body missing absolute WS-Addressing To: %s", body)
		}
		if !strings.Contains(body, "CIM_PowerManagementService") || !strings.Contains(body, "<p:ManagedElement>") {
			t.Fatalf("request body missing power service or managed element: %s", body)
		}
		_, _ = w.Write([]byte(`<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:ReturnValue>0</p:ReturnValue></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	client := New(server.URL, "", "")
	result := client.PowerOn(context.Background())
	if !result.OK || result.PowerState != bmc.PowerOn || result.Error != "" {
		t.Fatalf("PowerOn() = %+v, want ok on", result)
	}
}

func TestPowerOnReportsNonZeroReturnValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:ReturnValue>2</p:ReturnValue></s:Body></s:Envelope>`))
	}))
	defer server.Close()

	client := New(server.URL, "", "")
	result := client.PowerOn(context.Background())
	if result.OK || !strings.Contains(result.Error, "returned 2") {
		t.Fatalf("PowerOn() = %+v, want non-zero return error", result)
	}
}

func TestDigestChallengeRetry(t *testing.T) {
	challenges := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Re-challenge every unauthenticated request; the client retries each
		// POST (Enumerate, then Pull) with digest credentials.
		if r.Header.Get("Authorization") == "" {
			challenges++
			w.Header().Set("WWW-Authenticate", `Digest realm="AMT", nonce="abc", qop="auth"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Digest ") || !strings.Contains(auth, `username="admin"`) {
			t.Fatalf("Authorization = %q, want digest admin auth", auth)
		}
		switch r.Header.Get("SOAPAction") {
		case enumerateAction:
			_, _ = w.Write([]byte(`<s:Envelope xmlns:s="soap" xmlns:g="enum"><s:Body><g:EnumerateResponse><g:EnumerationContext>CTX-1</g:EnumerationContext></g:EnumerateResponse></s:Body></s:Envelope>`))
		case pullAction:
			_, _ = w.Write([]byte(`<s:Envelope xmlns:s="soap" xmlns:p="power"><s:Body><p:PullResponse><p:Items><p:CIM_AssociatedPowerManagementService><p:PowerState>2</p:PowerState></p:CIM_AssociatedPowerManagementService></p:Items></p:PullResponse></s:Body></s:Envelope>`))
		default:
			t.Fatalf("unexpected SOAPAction %q", r.Header.Get("SOAPAction"))
		}
	}))
	defer server.Close()

	client := New(server.URL, "admin", "password")
	result := client.GetPowerState(context.Background())
	if !result.OK || result.PowerState != bmc.PowerOn || challenges < 1 {
		t.Fatalf("GetPowerState() = %+v challenges=%d, want ok on after retry", result, challenges)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{host: "192.0.2.1", want: "http://192.0.2.1:16992/wsman"},
		{host: "192.0.2.1:16992", want: "http://192.0.2.1:16992/wsman"},
		{host: "http://192.0.2.1:16992", want: "http://192.0.2.1:16992/wsman"},
		{host: "https://192.0.2.1/wsman", want: "https://192.0.2.1/wsman"},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := normalizeEndpoint(tt.host); got != tt.want {
				t.Fatalf("normalizeEndpoint(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
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
