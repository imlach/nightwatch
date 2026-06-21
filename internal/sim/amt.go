package sim

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// WS-Man SOAPAction values and the requested-power-state codes the sim honors,
// matching internal/bmc/amtwsman.
const (
	amtEnumerateAction   = "http://schemas.xmlsoap.org/ws/2004/09/enumeration/Enumerate"
	amtPullAction        = "http://schemas.xmlsoap.org/ws/2004/09/enumeration/Pull"
	amtPowerChangeAction = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_PowerManagementService/RequestPowerStateChange"

	amtCodeOn       = 2
	amtCodeCycle    = 5
	amtCodeOffHard  = 6
	amtCodeOffSoft  = 8
	amtCodeReset    = 10
	amtReadStateOn  = 2 // CIM PowerState reading: On
	amtReadStateOff = 8 // CIM PowerState reading: Off-Soft
)

// AMTHandler is the transport-agnostic AMT sim: it serves the WS-Man HTTP
// endpoint at /wsman, holds an in-memory on/off power state, flips it on
// RequestPowerStateChange, and reports it via the Enumerate→Pull pair the real
// client issues. With a realm set it answers the first request with an HTTP
// digest challenge so the client's digest retry path is exercised.
type AMTHandler struct {
	mu        sync.Mutex
	on        bool
	realm     string // empty disables the digest challenge
	enumToken string
}

// NewAMTHandler builds the handler. If realm is non-empty the sim requires HTTP
// digest auth (any credentials accepted - the point is to drive the client's
// challenge/response, not to model AMT's user db).
func NewAMTHandler(initiallyOn bool, realm string) *AMTHandler {
	return &AMTHandler{on: initiallyOn, realm: realm, enumToken: "SIM-ENUM-CTX"}
}

// IsOn reports the current simulated power state.
func (h *AMTHandler) IsOn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.on
}

// SetPower forces the simulated power state out of band - e.g. to model the host
// powering itself off in response to an OS-level (Talos) shutdown.
func (h *AMTHandler) SetPower(on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.on = on
}

// ServeHTTP serves one WS-Man request. Mount it at /wsman.
func (h *AMTHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.realm != "" && r.Header.Get("Authorization") == "" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm=%q, nonce="sim-nonce", qop="auth"`, h.realm))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.Copy(io.Discard, r.Body)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", `application/soap+xml;charset="utf-8"`)
	switch r.Header.Get("SOAPAction") {
	case amtEnumerateAction:
		_, _ = io.WriteString(w, h.enumerateResponse())
	case amtPullAction:
		_, _ = io.WriteString(w, h.pullResponse())
	case amtPowerChangeAction:
		_, _ = io.WriteString(w, h.powerChangeResponse(string(body)))
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (h *AMTHandler) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/wsman", h)
	return mux
}

func (h *AMTHandler) enumerateResponse() string {
	return `<?xml version="1.0" encoding="UTF-8"?><a:Envelope xmlns:a="http://www.w3.org/2003/05/soap-envelope" xmlns:g="http://schemas.xmlsoap.org/ws/2004/09/enumeration"><a:Body><g:EnumerateResponse><g:EnumerationContext>` + h.enumToken + `</g:EnumerationContext></g:EnumerateResponse></a:Body></a:Envelope>`
}

// pullResponse mirrors the real CIM_AssociatedPowerManagementService Pull body,
// carrying the current PowerState (2=on, 8=off-soft) the client maps to on/off.
func (h *AMTHandler) pullResponse() string {
	h.mu.Lock()
	state := amtReadStateOff
	if h.on {
		state = amtReadStateOn
	}
	h.mu.Unlock()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><a:Envelope xmlns:a="http://www.w3.org/2003/05/soap-envelope" xmlns:g="http://schemas.xmlsoap.org/ws/2004/09/enumeration" xmlns:h="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_AssociatedPowerManagementService"><a:Body><g:PullResponse><g:Items><h:CIM_AssociatedPowerManagementService><h:PowerState>%d</h:PowerState></h:CIM_AssociatedPowerManagementService></g:Items><g:EndOfSequence></g:EndOfSequence></g:PullResponse></a:Body></a:Envelope>`, state)
}

// powerChangeResponse parses the requested PowerState, flips the in-memory state
// accordingly, and returns ReturnValue 0 (success), as real AMT does.
func (h *AMTHandler) powerChangeResponse(reqBody string) string {
	if code, ok := parsePowerStateInput(reqBody); ok {
		h.mu.Lock()
		switch code {
		case amtCodeOn, amtCodeCycle, amtCodeReset:
			h.on = true
		case amtCodeOffHard, amtCodeOffSoft:
			h.on = false
		}
		h.mu.Unlock()
	}
	return `<?xml version="1.0" encoding="UTF-8"?><a:Envelope xmlns:a="http://www.w3.org/2003/05/soap-envelope" xmlns:g="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_PowerManagementService"><a:Body><g:RequestPowerStateChange_OUTPUT><g:ReturnValue>0</g:ReturnValue></g:RequestPowerStateChange_OUTPUT></a:Body></a:Envelope>`
}

// parsePowerStateInput pulls the first <PowerState> value out of a
// RequestPowerStateChange_INPUT body.
func parsePowerStateInput(raw string) (int, bool) {
	dec := xml.NewDecoder(strings.NewReader(raw))
	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, false
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "PowerState" {
			continue
		}
		var v int
		if err := dec.DecodeElement(&v, &start); err != nil {
			return 0, false
		}
		return v, true
	}
}

// AMT is an httptest-backed AMT sim for in-process tests.
type AMT struct {
	*AMTHandler
	srv *httptest.Server
}

// NewAMT starts the sim with the given initial power state and digest realm.
func NewAMT(initiallyOn bool, realm string) *AMT {
	h := NewAMTHandler(initiallyOn, realm)
	return &AMT{AMTHandler: h, srv: httptest.NewServer(h.mux())}
}

// Endpoint returns the base URL to hand to amtwsman.New, which appends /wsman.
func (a *AMT) Endpoint() string { return a.srv.URL }

// Close shuts the server down.
func (a *AMT) Close() { a.srv.Close() }

// AMTServer is the fixed-port AMT sim for the container path.
type AMTServer struct {
	*AMTHandler
}

// NewAMTServer builds a fixed-port AMT sim.
func NewAMTServer(initiallyOn bool, realm string) *AMTServer {
	return &AMTServer{AMTHandler: NewAMTHandler(initiallyOn, realm)}
}

// HTTPServer returns an *http.Server bound to addr, mounting the WS-Man handler.
func (s *AMTServer) HTTPServer(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: s.mux()}
}
