// Package truenas talks to a TrueNAS SCALE box over its JSON-RPC 2.0 WebSocket
// API (wss://<host>/api/current). Nightwatch uses it for the storage side of the
// iSCSI safety gate (plan risk R5): before a worker loses power, the target
// storage target must show its initiator session gone. Reading sessions from the
// target - not the node - is what makes the gate trustworthy during a UPS
// load-shed, when the node may already be unreachable.
//
// SessionTable satisfies iscsi.SessionLister and is the gate's input. It returns
// the raw iscsi.global.sessions result as text rather than parsed fields, so the
// gate's case-insensitive substring match on the initiator IQN stays correct no
// matter how TrueNAS names its session fields - a parsed struct with a renamed
// field would render empty initiators and the gate would wrongly read "clear".
package truenas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/imlach/nightwatch/internal/iscsi"
)

// rpcCaller is the JSON-RPC transport the client needs. The real implementation
// is a WebSocket connection (wsConn); tests inject a fake.
type rpcCaller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Close() error
}

// Client is an authenticated TrueNAS JSON-RPC session.
type Client struct {
	rpc rpcCaller
}

// SessionTable is the gate's storage-side session lister.
var _ iscsi.SessionLister = (*Client)(nil).SessionTable

// New dials wss://<host>/api/current and authenticates with an API key. host is
// a host or host:port; username is the account the key belongs to.
func New(ctx context.Context, host, username, apiKey string, opts ...Option) (*Client, error) {
	conn, err := dial(ctx, host, opts...)
	if err != nil {
		return nil, err
	}
	c := &Client{rpc: conn}
	if err := c.login(ctx, username, apiKey); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func newWithRPC(rpc rpcCaller) *Client { return &Client{rpc: rpc} }

// login authenticates the connection with auth.login_ex (API_KEY_PLAIN).
func (c *Client) login(ctx context.Context, username, apiKey string) error {
	params := []any{map[string]string{
		"mechanism": "API_KEY_PLAIN",
		"username":  username,
		"api_key":   apiKey,
	}}
	raw, err := c.rpc.Call(ctx, "auth.login_ex", params)
	if err != nil {
		return fmt.Errorf("truenas: auth.login_ex: %w", err)
	}
	var res struct {
		ResponseType string `json:"response_type"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("truenas: decode login response: %w", err)
	}
	if res.ResponseType != "SUCCESS" {
		return fmt.Errorf("truenas: login failed: response_type=%q", res.ResponseType)
	}
	return nil
}

// Session mirrors the fields of an iscsi.global.sessions entry that are useful
// for logging. The gate does not depend on this struct - see SessionTable.
type Session struct {
	Initiator     string `json:"initiator"`
	InitiatorAddr string `json:"initiator_addr"`
	Target        string `json:"target"`
}

// Sessions returns the live iSCSI sessions decoded into structs (best-effort
// field mapping, for logging and diagnostics).
func (c *Client) Sessions(ctx context.Context) ([]Session, error) {
	raw, err := c.sessionsRaw(ctx)
	if err != nil {
		return nil, err
	}
	var out []Session
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("truenas: decode sessions: %w", err)
	}
	return out, nil
}

// SessionTable returns the raw iscsi.global.sessions result as indented JSON,
// satisfying iscsi.SessionLister. The gate substring-matches the initiator IQN
// against this text, so it is robust to TrueNAS session field naming.
func (c *Client) SessionTable(ctx context.Context) (string, error) {
	raw, err := c.sessionsRaw(ctx)
	if err != nil {
		return "", err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw), nil // fall back to compact form if not indentable
	}
	return pretty.String(), nil
}

func (c *Client) sessionsRaw(ctx context.Context) (json.RawMessage, error) {
	raw, err := c.rpc.Call(ctx, "iscsi.global.sessions", []any{})
	if err != nil {
		return nil, fmt.Errorf("truenas: iscsi.global.sessions: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("[]"), nil
	}
	return raw, nil
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	if c.rpc == nil {
		return nil
	}
	return c.rpc.Close()
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	msg := fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
	if d := strings.TrimSpace(string(e.Data)); d != "" && d != "null" {
		msg += ": " + d
	}
	return msg
}
