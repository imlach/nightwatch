package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// maxReadBytes caps a single JSON-RPC frame. The session list on a busy target
// is still small, but the coder/websocket default (32 KiB) is too tight to rely
// on, so we raise it generously.
const maxReadBytes = 16 << 20

// config holds dial-time options.
type config struct {
	tlsConfig *tls.Config
	insecure  bool
}

// Option configures the TrueNAS connection.
type Option func(*config)

// WithInsecureSkipVerify disables TLS certificate verification - needed when the
// target presents a self-signed cert (TrueNAS default) reached by IP.
func WithInsecureSkipVerify() Option { return func(c *config) { c.insecure = true } }

// WithTLSConfig overrides the TLS config used for the handshake (e.g. to trust
// an internal CA). Takes precedence over WithInsecureSkipVerify.
func WithTLSConfig(t *tls.Config) Option { return func(c *config) { c.tlsConfig = t } }

// wsConn is a JSON-RPC 2.0 connection over a single WebSocket. Calls are
// serialized so request/response correlation by id needs no demux read loop -
// Nightwatch's usage (login, then periodic session polls) is sequential.
type wsConn struct {
	ws *websocket.Conn
	mu sync.Mutex
	id int64
}

func dial(ctx context.Context, host string, opts ...Option) (*wsConn, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	tlsConf := cfg.tlsConfig
	if tlsConf == nil {
		tlsConf = &tls.Config{InsecureSkipVerify: cfg.insecure} //nolint:gosec // opt-in, internal target
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}}

	url := "wss://" + host + "/api/current"
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("truenas: dial %s: %w", url, err)
	}
	ws.SetReadLimit(maxReadBytes)
	return &wsConn{ws: ws}, nil
}

func (c *wsConn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	id := c.id
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	if err := wsjson.Write(ctx, c.ws, req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	for {
		var resp struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := wsjson.Read(ctx, c.ws, &resp); err != nil {
			return nil, fmt.Errorf("read %s response: %w", method, err)
		}
		if resp.ID == nil || *resp.ID != id {
			continue // server event push (e.g. collection_update), not our reply
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *wsConn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
