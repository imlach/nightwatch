// Package sim holds hardware-free simulators of the wire protocols Nightwatch's
// adapters speak - a TrueNAS JSON-RPC WebSocket server and an AMT WS-Man HTTP
// server. They let the real truenas/amtwsman clients (and the full lifecycle)
// be exercised end-to-end in CI against an in-memory backend, closing the gap
// left by in-process fakes that replace the client rather than the transport.
//
// Each sim splits into a transport-agnostic *Handler (the wire logic + state)
// and thin wrappers: an httptest server for in-process tests (New*) and a fixed
// -port *http.Server for the container path (cmd/nightwatch-sim).
package sim

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Session mirrors an iscsi.global.sessions entry. The gate only substring-matches
// on the rendered JSON, so any field carrying the initiator IP/IQN is sufficient.
type Session struct {
	Initiator     string `json:"initiator"`
	InitiatorAddr string `json:"initiator_addr"`
	Target        string `json:"target"`
}

// TrueNASHandler is the transport-agnostic TrueNAS sim: it serves JSON-RPC 2.0
// over a WebSocket at /api/current (auth.login_ex + iscsi.global.sessions)
// against an in-memory, mutex-guarded session list.
type TrueNASHandler struct {
	mu       sync.Mutex
	apiKey   string // expected API key; empty accepts any
	sessions []Session
}

// NewTrueNASHandler builds the handler with the given expected API key and
// initial sessions.
func NewTrueNASHandler(apiKey string, sessions ...Session) *TrueNASHandler {
	return &TrueNASHandler{apiKey: apiKey, sessions: sessions}
}

// SetSessions replaces the live session list.
func (h *TrueNASHandler) SetSessions(sessions ...Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions = sessions
}

// RemoveSessionByAddr drops every session whose initiator_addr matches addr,
// modelling a node logging out its iSCSI sessions as it powers down.
func (h *TrueNASHandler) RemoveSessionByAddr(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	kept := h.sessions[:0:0]
	for _, s := range h.sessions {
		if s.InitiatorAddr != addr {
			kept = append(kept, s)
		}
	}
	h.sessions = kept
}

// ServeHTTP upgrades to a WebSocket and serves JSON-RPC calls until the client
// closes. Mount it at /api/current.
func (h *TrueNASHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(16 << 20)
	ctx := r.Context()
	for {
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := wsjson.Read(ctx, ws, &req); err != nil {
			return // client closed
		}
		result, rpcErr := h.dispatch(req.Method, req.Params)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := wsjson.Write(ctx, ws, resp); err != nil {
			return
		}
	}
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (h *TrueNASHandler) dispatch(method string, params json.RawMessage) (any, *rpcErr) {
	switch method {
	case "auth.login_ex":
		return h.login(params)
	case "iscsi.global.sessions":
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]Session(nil), h.sessions...), nil
	default:
		return nil, &rpcErr{Code: -32601, Message: "method not found: " + method}
	}
}

func (h *TrueNASHandler) login(params json.RawMessage) (any, *rpcErr) {
	var args []struct {
		Mechanism string `json:"mechanism"`
		Username  string `json:"username"`
		APIKey    string `json:"api_key"`
	}
	if err := json.Unmarshal(params, &args); err != nil || len(args) == 0 {
		return nil, &rpcErr{Code: -32602, Message: "invalid login params"}
	}
	a := args[0]
	if a.Mechanism != "API_KEY_PLAIN" {
		return map[string]string{"response_type": "AUTH_ERR"}, nil
	}
	h.mu.Lock()
	want := h.apiKey
	h.mu.Unlock()
	if want != "" && a.APIKey != want {
		return map[string]string{"response_type": "AUTH_ERR"}, nil
	}
	return map[string]string{"response_type": "SUCCESS"}, nil
}

func (h *TrueNASHandler) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/current", h)
	return mux
}

// TrueNAS is an httptest-backed TrueNAS sim for in-process tests: a TLS
// WebSocket server with a freshly generated self-signed cert.
type TrueNAS struct {
	*TrueNASHandler
	srv *httptest.Server
}

// NewTrueNAS starts a TLS WebSocket server with the given initial sessions.
// apiKey, if non-empty, is the only key auth.login_ex accepts. Close it via
// Close.
func NewTrueNAS(apiKey string, sessions ...Session) *TrueNAS {
	h := NewTrueNASHandler(apiKey, sessions...)
	cert, _ := selfSignedCert()
	srv := httptest.NewUnstartedServer(h.mux())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	return &TrueNAS{TrueNASHandler: h, srv: srv}
}

// Host returns the host:port the client should dial (the wss:// scheme and
// /api/current path are added by truenas.New).
func (t *TrueNAS) Host() string { return t.srv.Listener.Addr().String() }

// Close shuts the server down.
func (t *TrueNAS) Close() { t.srv.Close() }

// TrueNASServer is the fixed-port TrueNAS sim for the container path. Build the
// *http.Server with HTTPServer and serve it over TLS with the cert PEMs.
type TrueNASServer struct {
	*TrueNASHandler
	certPEM, keyPEM []byte
}

// NewTrueNASServer builds a fixed-port sim with a fresh self-signed cert.
func NewTrueNASServer(apiKey string, sessions ...Session) *TrueNASServer {
	h := NewTrueNASHandler(apiKey, sessions...)
	certPEM, keyPEM := selfSignedPEM()
	return &TrueNASServer{TrueNASHandler: h, certPEM: certPEM, keyPEM: keyPEM}
}

// HTTPServer returns an *http.Server bound to addr, mounting the JSON-RPC handler.
func (s *TrueNASServer) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.mux()}
}

// TLSCertPEM / TLSKeyPEM return the PEM-encoded self-signed cert + key.
func (s *TrueNASServer) TLSCertPEM() []byte { return s.certPEM }
func (s *TrueNASServer) TLSKeyPEM() []byte  { return s.keyPEM }

// selfSignedCert generates an ephemeral self-signed cert covering localhost.
func selfSignedCert() (tls.Certificate, []byte) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "nightwatch-sim"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		panic(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, keyDER
}

func selfSignedPEM() (certPEM, keyPEM []byte) {
	cert, keyDER := selfSignedCert()
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}
