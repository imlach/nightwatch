// Package api is Nightwatch's token-authed HTTP trigger surface: a remote
// scheduler POSTs to drive the same drain-shutdown / wake / status path the CLI
// uses (via internal/operate), single named node only - no broadcast, matching
// the CLI's sharpness. The actuator can power off nodes, so auth fails closed
// (no token configured ⇒ refuse to start; see NewServer).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/operate"
	"github.com/imlach/nightwatch/internal/operation"
)

// Defaults mirror the CLI flag defaults so the API behaves like a CLI invocation.
const (
	defaultDrainTimeout     = 5 * time.Minute
	defaultStorageTimeout   = 2 * time.Minute
	defaultPowerOffTimeout  = 5 * time.Minute
	defaultReachableTimeout = 5 * time.Minute
	defaultReadyTimeout     = 5 * time.Minute
	defaultGPUTimeout       = 3 * time.Minute
	defaultPoll             = 5 * time.Second
	defaultStatusTimeout    = 10 * time.Second
)

// Handler holds the dependencies the HTTP routes drive. Builder is the
// operate.Builder seam (RealBuilder in production; a fake in tests) so handler
// tests run the lifecycle without touching the network.
type Handler struct {
	Inv     *inventory.Inventory
	Token   string
	Builder operate.Builder
	locks   actuatorLocks

	// Kubeconfig/Talosconfig are threaded into the per-op operate.Config.
	Kubeconfig, Talosconfig string

	// StatusTimeout bounds a /status BMC read; zero ⇒ defaultStatusTimeout.
	StatusTimeout time.Duration

	// reachable is an optional best-effort Talos reachability probe for /status;
	// nil ⇒ reachability is omitted (it needs a live talosconfig + client).
	reachable func(ctx context.Context, endpoint string) (bool, error)
}

// drainShutdownReq / wakeReq are the optional JSON bodies. Absent body ⇒ defaults.
type drainShutdownReq struct {
	ForceBMCOff *bool `json:"forceBmcOff,omitempty"`
	DryRun      *bool `json:"dryRun,omitempty"`
}

type wakeReq struct {
	SkipGPUWait *bool `json:"skipGpuWait,omitempty"`
	DryRun      *bool `json:"dryRun,omitempty"`
}

// opResponse is the result shape for drain-shutdown / wake.
type opResponse struct {
	OK    bool             `json:"ok"`
	Node  string           `json:"node"`
	Steps []operation.Step `json:"steps,omitempty"`
	Error string           `json:"error,omitempty"`
}

type statusResponse struct {
	Node      string `json:"node"`
	BMCPower  string `json:"bmcPower"`
	BMCError  string `json:"bmcError,omitempty"`
	Reachable *bool  `json:"reachable,omitempty"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Routes builds the mux: /healthz is unauthenticated; every /v1/ route is
// wrapped in bearer-token auth.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /v1/nodes/{node}/drain-shutdown", h.auth(http.HandlerFunc(h.drainShutdown)))
	mux.Handle("POST /v1/nodes/{node}/wake", h.auth(http.HandlerFunc(h.wake)))
	mux.Handle("GET /v1/nodes/{node}/status", h.auth(http.HandlerFunc(h.status)))
	return mux
}

// auth enforces a constant-time bearer-token match on /v1 requests.
func (h *Handler) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		got := r.Header.Get("Authorization")
		if len(got) <= len(prefix) || got[:len(prefix)] != prefix ||
			subtle.ConstantTimeCompare([]byte(got[len(prefix):]), []byte(h.Token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) builder() operate.Builder {
	if h.Builder != nil {
		return h.Builder
	}
	return operate.RealBuilder
}

func (h *Handler) drainShutdown(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	var req drainShutdownReq
	if !decodeBody(w, r, &req) {
		return
	}
	cfg := operate.Config{
		Kubeconfig:      h.Kubeconfig,
		Talosconfig:     h.Talosconfig,
		ForceBMCOff:     boolOr(req.ForceBMCOff, false),
		DrainTimeout:    defaultDrainTimeout,
		StorageTimeout:  defaultStorageTimeout,
		PowerOffTimeout: defaultPowerOffTimeout,
		Poll:            defaultPoll,
	}
	if boolOr(req.DryRun, false) {
		h.writeDryRun(w, node)
		return
	}
	release, ok := h.locks.tryAcquire(node)
	if !ok {
		h.writeActuatorConflict(w, node)
		return
	}
	defer release()
	steps, err := operate.DrainShutdown(r.Context(), h.Inv, node, cfg, h.builder())
	h.writeOpResult(w, node, steps, err)
}

func (h *Handler) wake(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	var req wakeReq
	if !decodeBody(w, r, &req) {
		return
	}
	cfg := operate.Config{
		Kubeconfig:       h.Kubeconfig,
		Talosconfig:      h.Talosconfig,
		SkipGPUWait:      boolOr(req.SkipGPUWait, false),
		ReachableTimeout: defaultReachableTimeout,
		ReadyTimeout:     defaultReadyTimeout,
		GPUTimeout:       defaultGPUTimeout,
		Poll:             defaultPoll,
	}
	if boolOr(req.DryRun, false) {
		h.writeDryRun(w, node)
		return
	}
	release, ok := h.locks.tryAcquire(node)
	if !ok {
		h.writeActuatorConflict(w, node)
		return
	}
	defer release()
	steps, err := operate.Wake(r.Context(), h.Inv, node, cfg, h.builder())
	h.writeOpResult(w, node, steps, err)
}

// status is a best-effort read: the node's BMC power state (always) and Talos
// reachability (when a probe is wired). It never powers anything.
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	node := r.PathValue("node")
	spec, err := operate.Lookup(h.Inv, node)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	timeout := h.StatusTimeout
	if timeout <= 0 {
		timeout = defaultStatusTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	resp := statusResponse{Node: node, BMCPower: string(bmc.PowerUnknown)}
	if client, berr := bmc.New(spec.BMC.Type, spec.BMC.Host, spec.BMC.Username, spec.BMC.Password); berr != nil {
		resp.BMCError = berr.Error()
	} else if res := client.GetPowerState(ctx); res.OK {
		resp.BMCPower = string(res.PowerState)
	} else {
		resp.BMCError = res.Error
	}
	if h.reachable != nil {
		if ok, rerr := h.reachable(ctx, spec.TalosEndpoint); rerr == nil {
			resp.Reachable = &ok
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) writeDryRun(w http.ResponseWriter, node string) {
	if _, err := operate.Lookup(h.Inv, node); err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, opResponse{OK: true, Node: node})
}

func (h *Handler) writeActuatorConflict(w http.ResponseWriter, node string) {
	writeJSON(w, http.StatusConflict, opResponse{
		OK:    false,
		Node:  node,
		Error: fmt.Sprintf("node %q already has an actuator operation in progress", node),
	})
}

// writeOpResult maps an operate result to the response: 200 on success; 404 for
// an unknown node; 409 when steps ran but the op failed (a real lifecycle stop);
// 500 for an assembly error before any step. The steps are always returned so
// the caller sees how far it got.
func (h *Handler) writeOpResult(w http.ResponseWriter, node string, steps []operation.Step, err error) {
	if err == nil {
		writeJSON(w, http.StatusOK, opResponse{OK: true, Node: node, Steps: steps})
		return
	}
	var unknown *operate.UnknownNodeError
	if errors.As(err, &unknown) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	code := http.StatusInternalServerError // assembly failed before any step ran
	if len(steps) > 0 {
		code = http.StatusConflict // the lifecycle ran and stopped at a failed step
	}
	writeJSON(w, code, opResponse{OK: false, Node: node, Steps: steps, Error: err.Error()})
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true // empty body ⇒ defaults
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid request body: %v", err)})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
