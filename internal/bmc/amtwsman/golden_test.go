package amtwsman

// Golden fixtures pinning the AMT WS-Man contract proved live against
// node-1 (example AMT endpoint). testdata/*_response.xml and the
// fault are real firmware bodies; testdata/*_request.golden.xml are the exact
// envelopes this client emits. Regenerate requests with:
//   go test ./internal/bmc/amtwsman -run TestGolden -update-golden
// Responses are hand-captured - do not regenerate them.

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/imlach/nightwatch/internal/bmc"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/*_request.golden.xml from generated envelopes")

// msgIDRe matches the random WS-Addressing MessageID this client generates
// ("uuid:" + 16 random bytes hex); normalized so request goldens are stable.
var msgIDRe = regexp.MustCompile(`uuid:[0-9a-f]{32}`)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// normalizeRequest makes a captured request envelope deterministic: the
// httptest port (which varies per run) becomes http://AMT and the random
// MessageID becomes a fixed token.
func normalizeRequest(body, serverURL string) string {
	body = strings.ReplaceAll(body, serverURL, "http://AMT")
	return msgIDRe.ReplaceAllString(body, "uuid:MESSAGE-ID")
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", name, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update-golden to create)", name, err)
	}
	if got != string(want) {
		t.Fatalf("request envelope mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestGoldenResponseParsing pins how the parsers read the real firmware bodies.
func TestGoldenResponseParsing(t *testing.T) {
	if state := ParsePowerState(readFixture(t, "pull_response.xml")); state != bmc.PowerOn {
		t.Fatalf("pull_response PowerState = %q, want on", state)
	}
	if ctx, ok := parseStringElement(readFixture(t, "enumerate_response.xml"), "EnumerationContext"); !ok || ctx != "F7060000-0000-0000-0000-000000000000" {
		t.Fatalf("enumerate_response EnumerationContext = %q ok=%v", ctx, ok)
	}
	if rv, ok := parseReturnValue(readFixture(t, "request_power_state_change_output.xml")); !ok || rv != 0 {
		t.Fatalf("power-change ReturnValue = %d ok=%v, want 0", rv, ok)
	}
	if state := ParsePowerState(readFixture(t, "fault_invalid_resource_uri.xml")); state != bmc.PowerUnknown {
		t.Fatalf("fault PowerState = %q, want unknown", state)
	}
}

// TestGoldenReadRequests runs GetPowerState against the golden enumerate+pull
// responses and pins the two request envelopes it emits.
func TestGoldenReadRequests(t *testing.T) {
	var enumReq, pullReq string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch r.Header.Get("SOAPAction") {
		case enumerateAction:
			enumReq = body
			_, _ = io.WriteString(w, readFixture(t, "enumerate_response.xml"))
		case pullAction:
			pullReq = body
			_, _ = io.WriteString(w, readFixture(t, "pull_response.xml"))
		default:
			t.Fatalf("unexpected SOAPAction %q", r.Header.Get("SOAPAction"))
		}
	}))
	defer server.Close()

	result := New(server.URL, "", "").GetPowerState(context.Background())
	if !result.OK || result.PowerState != bmc.PowerOn {
		t.Fatalf("GetPowerState() = %+v, want ok on", result)
	}
	assertGolden(t, "enumerate_request.golden.xml", normalizeRequest(enumReq, server.URL))
	assertGolden(t, "pull_request.golden.xml", normalizeRequest(pullReq, server.URL))
}

// TestGoldenPowerChangeRequest pins the RequestPowerStateChange envelope (reset).
func TestGoldenPowerChangeRequest(t *testing.T) {
	var req string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req = readBody(t, r)
		if r.Header.Get("SOAPAction") != powerChangeAction {
			t.Fatalf("SOAPAction = %q, want %q", r.Header.Get("SOAPAction"), powerChangeAction)
		}
		_, _ = io.WriteString(w, readFixture(t, "request_power_state_change_output.xml"))
	}))
	defer server.Close()

	result := New(server.URL, "", "").Reset(context.Background())
	if !result.OK || result.PowerState != bmc.PowerOn {
		t.Fatalf("Reset() = %+v, want ok on", result)
	}
	assertGolden(t, "power_change_request.golden.xml", normalizeRequest(req, server.URL))
}
