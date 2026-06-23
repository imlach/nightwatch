package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imlach/nightwatch/internal/bmc"
	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/k8s"
	"github.com/imlach/nightwatch/internal/lifecycle"
	"github.com/imlach/nightwatch/internal/operate"
)

const invYAML = `
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
`

const token = "test-token"

func loadInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Load([]byte(invYAML))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

// --- fakes satisfying the operate backend interfaces (no network) ---

type fakeNodes struct{}

func (fakeNodes) Cordon(context.Context, string) error                     { return nil }
func (fakeNodes) Drain(context.Context, string, k8s.DrainOptions) error    { return nil }
func (fakeNodes) Uncordon(context.Context, string) error                   { return nil }
func (fakeNodes) IsNodeReady(context.Context, string) (bool, error)        { return true, nil }
func (fakeNodes) NodeHasGPUCapacity(context.Context, string) (bool, error) { return true, nil }
func (fakeNodes) IsNodeSchedulable(context.Context, string) (bool, error)  { return true, nil }

type fakePower struct{ on bool }

func (f *fakePower) GetPowerState(context.Context) bmc.Result {
	st := bmc.PowerOff
	if f.on {
		st = bmc.PowerOn
	}
	return bmc.Result{OK: true, PowerState: st}
}
func (f *fakePower) PowerOn(context.Context) bmc.Result {
	f.on = true
	return bmc.Result{OK: true, PowerState: bmc.PowerOn}
}
func (f *fakePower) SoftOff(context.Context) bmc.Result { return bmc.Result{OK: true} }
func (f *fakePower) HardOff(context.Context) bmc.Result {
	f.on = false
	return bmc.Result{OK: true, PowerState: bmc.PowerOff}
}
func (f *fakePower) Reset(context.Context) bmc.Result { return bmc.Result{OK: true} }

type fakeTalos struct{ power *fakePower }

func (f *fakeTalos) Shutdown(context.Context, string) error { f.power.on = false; return nil }
func (f *fakeTalos) Reachable(context.Context, string) bool { return true }

// fakeBuilder gives the handler converging backends with no network.
func fakeBuilder() operate.Builder {
	return func(context.Context, inventory.NodeSpec, operate.Config) (operate.Backends, error) {
		power := &fakePower{on: true}
		return operate.Backends{
			Nodes:   fakeNodes{},
			Talos:   &fakeTalos{power: power},
			Power:   power,
			Storage: lifecycle.StorageGateFunc(func(context.Context, time.Duration) error { return nil }),
		}, nil
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	h := &Handler{Inv: loadInv(t), Token: token, Builder: fakeBuilder()}
	return h.Routes()
}

func loadTwoNodeInv(t *testing.T) *inventory.Inventory {
	t.Helper()
	inv, err := inventory.Load([]byte(`
nodes:
  node-1:
    talos_endpoint: "192.0.2.10"
    kube_node_name: node-1
    bmc: {type: amt, host: "192.0.2.1"}
  node-2:
    talos_endpoint: "192.0.2.11"
    kube_node_name: node-2
    bmc: {type: amt, host: "192.0.2.2"}
`))
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

type blockingOperateBuilder struct {
	started chan string
	release chan struct{}
}

func newBlockingOperateBuilder() *blockingOperateBuilder {
	return &blockingOperateBuilder{
		started: make(chan string, 4),
		release: make(chan struct{}),
	}
}

func (b *blockingOperateBuilder) builder() operate.Builder {
	return func(ctx context.Context, node inventory.NodeSpec, cfg operate.Config) (operate.Backends, error) {
		b.started <- node.KubeNodeName
		select {
		case <-b.release:
		case <-ctx.Done():
			return operate.Backends{}, ctx.Err()
		}
		power := &fakePower{on: true}
		return operate.Backends{
			Nodes:   fakeNodes{},
			Talos:   &fakeTalos{power: power},
			Power:   power,
			Storage: lifecycle.StorageGateFunc(func(context.Context, time.Duration) error { return nil }),
		}, nil
	}
}

func req(method, path, body, bearer string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func TestHealthzNoAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("GET", "/healthz", "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestDrainShutdownMissingToken(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", rec.Code)
	}
}

func TestDrainShutdownWrongToken(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", "", "wrong"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", rec.Code)
	}
}

func TestDrainShutdownUnknownNode(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/nope/drain-shutdown", "", token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d, want 404", rec.Code)
	}
}

func TestDrainShutdownHappyPath(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", "", token))
	if rec.Code != http.StatusOK {
		t.Fatalf("drain-shutdown = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp opResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false: %s", resp.Error)
	}
	if resp.Node != "node-1" {
		t.Errorf("node = %q", resp.Node)
	}
	if len(resp.Steps) == 0 {
		t.Error("no steps returned")
	}
	if last := resp.Steps[len(resp.Steps)-1]; last.Name != "wait-power-off" {
		t.Errorf("final step = %q, want wait-power-off", last.Name)
	}
}

func TestDrainShutdownBodyForceBMCOff(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", `{"forceBmcOff":true}`, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("with body = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDrainShutdownDryRun(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", `{"dryRun":true}`, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run = %d, want 200", rec.Code)
	}
	var resp opResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.OK || len(resp.Steps) != 0 {
		t.Errorf("dry-run should be ok with no steps, got %+v", resp)
	}
}

func TestActuatorRejectsConcurrentSameNodeRequest(t *testing.T) {
	blocking := newBlockingOperateBuilder()
	h := (&Handler{Inv: loadInv(t), Token: token, Builder: blocking.builder()}).Routes()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", "", token))
		done <- rec
	}()

	select {
	case got := <-blocking.started:
		if got != "node-1" {
			t.Fatalf("started node = %q, want node-1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first actuator request did not start")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("POST", "/v1/nodes/node-1/wake", "", token))
	if rec.Code != http.StatusConflict {
		t.Fatalf("same-node concurrent actuator = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already has an actuator operation") {
		t.Fatalf("conflict body = %s", rec.Body.String())
	}

	close(blocking.release)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("first actuator request = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first actuator request did not finish")
	}
}

func TestActuatorAllowsConcurrentDifferentNodes(t *testing.T) {
	blocking := newBlockingOperateBuilder()
	h := (&Handler{Inv: loadTwoNodeInv(t), Token: token, Builder: blocking.builder()}).Routes()

	done1 := make(chan *httptest.ResponseRecorder, 1)
	done2 := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("POST", "/v1/nodes/node-1/drain-shutdown", "", token))
		done1 <- rec
	}()
	if got := waitStarted(t, blocking.started); got != "node-1" {
		t.Fatalf("first started node = %q, want node-1", got)
	}

	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("POST", "/v1/nodes/node-2/drain-shutdown", "", token))
		done2 <- rec
	}()
	if got := waitStarted(t, blocking.started); got != "node-2" {
		t.Fatalf("second started node = %q, want node-2", got)
	}

	close(blocking.release)
	for name, done := range map[string]chan *httptest.ResponseRecorder{"node-1": done1, "node-2": done2} {
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Fatalf("%s actuator request = %d, want 200; body=%s", name, rec.Code, rec.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatalf("%s actuator request did not finish", name)
		}
	}
}

func waitStarted(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case node := <-ch:
		return node
	case <-time.After(time.Second):
		t.Fatal("actuator request did not start")
	}
	return ""
}

func TestWakeHappyPath(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/node-1/wake", `{"skipGpuWait":true}`, token))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp opResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Errorf("ok = false: %s", resp.Error)
	}
	if last := resp.Steps[len(resp.Steps)-1]; last.Name != "uncordon" {
		t.Errorf("final step = %q, want uncordon", last.Name)
	}
}

func TestWakeUnknownNode(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("POST", "/v1/nodes/nope/wake", "", token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node = %d, want 404", rec.Code)
	}
}

func TestStatusUnknownNode(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("GET", "/v1/nodes/nope/status", "", token))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status unknown = %d, want 404", rec.Code)
	}
}

func TestStatusNoAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req("GET", "/v1/nodes/node-1/status", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status no token = %d, want 401", rec.Code)
	}
}

func TestServeRefusesWithoutToken(t *testing.T) {
	err := Serve(context.Background(), loadInv(t), ServeConfig{Listen: "127.0.0.1:0", Token: ""})
	if err == nil {
		t.Fatal("Serve with no token should refuse to start")
	}
	if !strings.Contains(err.Error(), "NIGHTWATCH_API_TOKEN") {
		t.Errorf("err = %v, want it to mention NIGHTWATCH_API_TOKEN", err)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, loadInv(t), ServeConfig{Listen: "127.0.0.1:0", Token: token}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}
