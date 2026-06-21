package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/imlach/nightwatch/internal/iscsi"
)

const wkr01IQN = "iqn.2005-03.org.open-iscsi:node-1"

type fakeRPC struct {
	results map[string]json.RawMessage
	errs    map[string]error
	calls   []string
	params  map[string]any
}

func (f *fakeRPC) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.calls = append(f.calls, method)
	if f.params == nil {
		f.params = map[string]any{}
	}
	f.params[method] = params
	if e := f.errs[method]; e != nil {
		return nil, e
	}
	return f.results[method], nil
}

func (f *fakeRPC) Close() error { return nil }

func TestLoginSuccess(t *testing.T) {
	f := &fakeRPC{results: map[string]json.RawMessage{
		"auth.login_ex": json.RawMessage(`{"response_type":"SUCCESS"}`),
	}}
	c := newWithRPC(f)
	if err := c.login(context.Background(), "nightwatch", "1-secret"); err != nil {
		t.Fatalf("login = %v, want nil", err)
	}
	// API_KEY_PLAIN mechanism + api_key field must be sent (not "key").
	sent, _ := json.Marshal(f.params["auth.login_ex"])
	for _, want := range []string{"API_KEY_PLAIN", `"api_key"`, "1-secret", "nightwatch"} {
		if !strings.Contains(string(sent), want) {
			t.Errorf("login params %s missing %q", sent, want)
		}
	}
}

func TestLoginDenied(t *testing.T) {
	f := &fakeRPC{results: map[string]json.RawMessage{
		"auth.login_ex": json.RawMessage(`{"response_type":"AUTH_ERR"}`),
	}}
	if err := newWithRPC(f).login(context.Background(), "u", "bad"); err == nil {
		t.Fatal("login = nil, want error on non-SUCCESS response_type")
	}
}

func TestLoginRPCError(t *testing.T) {
	f := &fakeRPC{errs: map[string]error{"auth.login_ex": errors.New("conn reset")}}
	if err := newWithRPC(f).login(context.Background(), "u", "k"); err == nil {
		t.Fatal("login = nil, want wrapped transport error")
	}
}

// The load-bearing safety property: the rendered table must let the gate see a
// present initiator IQN, and report clear once it is gone.
func TestSessionTableFeedsGate(t *testing.T) {
	withSession := json.RawMessage(`[{"initiator":"` + wkr01IQN + `","initiator_addr":"192.0.2.10","target":"iqn.2011-08.com.example:tank/k3s"}]`)
	f := &fakeRPC{results: map[string]json.RawMessage{"iscsi.global.sessions": withSession}}
	table, err := newWithRPC(f).SessionTable(context.Background())
	if err != nil {
		t.Fatalf("SessionTable = %v, want nil", err)
	}
	if !iscsi.SessionPresent(table, wkr01IQN) {
		t.Fatalf("gate did not see present session in table:\n%s", table)
	}

	f.results["iscsi.global.sessions"] = json.RawMessage(`[]`)
	table, err = newWithRPC(f).SessionTable(context.Background())
	if err != nil {
		t.Fatalf("SessionTable (empty) = %v, want nil", err)
	}
	if iscsi.SessionPresent(table, wkr01IQN) {
		t.Fatalf("gate saw a session in an empty table:\n%s", table)
	}
}

// A renamed initiator field must not break detection - the raw text still
// carries the IQN, which is why SessionTable returns raw JSON, not parsed structs.
func TestSessionTableRobustToFieldNames(t *testing.T) {
	odd := json.RawMessage(`[{"initiator_name":"` + wkr01IQN + `","addr":"192.0.2.10"}]`)
	f := &fakeRPC{results: map[string]json.RawMessage{"iscsi.global.sessions": odd}}
	table, err := newWithRPC(f).SessionTable(context.Background())
	if err != nil {
		t.Fatalf("SessionTable = %v, want nil", err)
	}
	if !iscsi.SessionPresent(table, wkr01IQN) {
		t.Fatalf("gate must still see the IQN regardless of field naming:\n%s", table)
	}
}

func TestSessionTableEmptyResult(t *testing.T) {
	// A null/empty result must render as no sessions, not error.
	f := &fakeRPC{results: map[string]json.RawMessage{"iscsi.global.sessions": json.RawMessage(`null`)}}
	table, err := newWithRPC(f).SessionTable(context.Background())
	if err != nil {
		t.Fatalf("SessionTable = %v, want nil", err)
	}
	if iscsi.SessionPresent(table, wkr01IQN) {
		t.Fatalf("null result should be clear, got:\n%s", table)
	}
}

func TestSessions(t *testing.T) {
	raw := json.RawMessage(`[{"initiator":"` + wkr01IQN + `","initiator_addr":"192.0.2.10","target":"t"}]`)
	f := &fakeRPC{results: map[string]json.RawMessage{"iscsi.global.sessions": raw}}
	got, err := newWithRPC(f).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Initiator != wkr01IQN || got[0].InitiatorAddr != "192.0.2.10" {
		t.Fatalf("Sessions = %+v, want one wkr-01 session", got)
	}
}

func TestSessionsRPCError(t *testing.T) {
	f := &fakeRPC{errs: map[string]error{"iscsi.global.sessions": errors.New("boom")}}
	if _, err := newWithRPC(f).SessionTable(context.Background()); err == nil {
		t.Fatal("SessionTable = nil, want wrapped RPC error (must not read as clear)")
	}
}

func TestRPCErrorMessage(t *testing.T) {
	e := &rpcError{Code: 22, Message: "EINVAL", Data: json.RawMessage(`"detail"`)}
	if got := e.Error(); !strings.Contains(got, "22") || !strings.Contains(got, "EINVAL") || !strings.Contains(got, "detail") {
		t.Fatalf("rpcError.Error() = %q, missing code/message/data", got)
	}
}
