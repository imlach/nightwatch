# nightwatch

Bare-metal node lifecycle orchestrator for Talos-based workload clusters: graceful
drain → iSCSI storage gate → OS shutdown → BMC power-off (and the wake path
back). Talks to TrueNAS (JSON-RPC over WebSocket), Intel AMT (WS-Man over HTTP
digest) and Dell iDRAC9 (Redfish over HTTPS), the Talos machine API (mTLS gRPC),
and Kubernetes.

This is a personal infrastructure tool published in the open for transparency and
reuse. It is not packaged as a supported OSS product.

## Images and releases

CI publishes two images to GHCR on every push to `main` and on version tags:

- `ghcr.io/imlach/nightwatch` - operator + CLI + `serve` API
- `ghcr.io/imlach/nightwatch-sim` - protocol simulators for the integration tests

`main` is a rolling tag; pushing a `vX.Y.Z` git tag publishes the immutable
`:vX.Y.Z` and `:latest`. Consumers should pin by digest. CI builds run on
GitHub-hosted runners (no hardware or cluster access required).

## BMC drivers

Drivers self-register by `bmc.type`: `amt` (`internal/bmc/amtwsman`) and
`redfish`/`idrac` (`internal/bmc/redfish`). The Redfish driver uses HTTP Basic
auth and skips TLS verify for iDRAC's self-signed cert.

## Operator (ElasticNode reconciler)

`cmd/nightwatch-operator` is a leader-elected controller that runs on the
**management** cluster and reconciles `ElasticNode` CRs (`api/v1alpha1`) toward
`spec.desiredPower` (`On`/`Off`, default `On`). Each loop reads the live state -
BMC power and the target node's Ready/GPU status - then drives the
`lifecycle.DrainShutdown` / `lifecycle.PowerOn` state machines to converge.
`status` is observability only; it never feeds back into the next decision.

It uses two Kubernetes clients: one for the management cluster (where the CRs
live) and a separate target-cluster client (from the `Backends` provider) for the
cordon/drain/Ready/GPU checks on the cluster being scaled.

CRD + RBAC manifests are in `config/`. The cross-cluster `Backends` provider
(`internal/controller/provider.go`) builds the target-cluster client and the
per-node BMC/Talos/iSCSI adapters from the inventory ConfigMap + credentials Secret.

### Observability

The operator serves standard controller-runtime + Go runtime metrics on
`--metrics-bind-address` (default `:8080`); it registers no custom metrics. A
portable Grafana dashboard for these is in [`dashboards/`](dashboards/) — see
[`dashboards/README.md`](dashboards/README.md). It binds to any Prometheus via
template variables and carries no cluster-specific labels.

## HTTP trigger API (`serve`)

`nightwatch serve` exposes the same drain-shutdown / wake / status path the CLI
drives (shared `internal/operate` layer) so a remote scheduler or CronJob can
POST to drive the actuator.

- `--listen` (env `NIGHTWATCH_LISTEN`, default `:8080`).
- **Auth fails closed**: `NIGHTWATCH_API_TOKEN` must be set or `serve` refuses to
  start (the endpoint can power off nodes). Every `/v1/*` request needs
  `Authorization: Bearer <token>` (constant-time compared); `/healthz` is open.
- One named node per request; no broadcast.

| Method + path | Body (optional) | Result |
| --- | --- | --- |
| `POST /v1/nodes/{node}/drain-shutdown` | `{forceBmcOff?, dryRun?}` | `200 {ok,node,steps[]}` · `404` unknown · `409` op stopped · `500` assembly |
| `POST /v1/nodes/{node}/wake` | `{skipGpuWait?, dryRun?}` | as above (final step `uncordon`) |
| `GET /v1/nodes/{node}/status` | - | `{node,bmcPower,reachable?}` (best-effort) |
| `GET /healthz` | - | `200` |

Lifecycle ops run **synchronously** inside the request (a drain takes minutes),
so the server write timeout is generous (20m) and the caller must set a matching
long client timeout. SIGINT/SIGTERM drains in-flight requests, then exits.

## Tests

### Unit tests (`make test`)

Fast, no network. Real adapter clients run against in-process fakes and golden
fixtures:

- **AMT** - golden SOAP/XML fixtures for the WS-Man wire format
  (`internal/bmc/amtwsman/testdata`).
- **Redfish/iDRAC** - the real client parses canned iDRAC Redfish responses
  (`internal/bmc/redfish/testdata`): power state, the reset action, URL handling.

### Integration tests (`make test-integration`)

`go test -tags=integration ./...`. The **real** adapter clients talk to protocol
simulators (`internal/sim`), so the actual wire code runs end to end against an
in-memory backend. No hardware, privileges, or egress - the sims bind loopback,
and the `integration` build tag keeps this out of `make test`.

- **TrueNAS** (`internal/sim/truenas.go`) - a `wss://` JSON-RPC 2.0 server with a
  self-signed cert. The real client logs in over TLS and its session table feeds
  the real `iscsi.Gate`; dropping a session makes the gate report clear.
- **AMT** (`internal/sim/amt.go`) - a WS-Man HTTP server with digest auth and
  in-memory power state. The real client reads power via Enumerate->Pull and
  flips it through the digest challenge/retry path.
- **Full loop** - `lifecycle.DrainShutdown` end to end against the TrueNAS + AMT
  sims, with in-process fakes for Kubernetes and Talos, asserting power ends off
  and the storage gate is clear.

Redfish has no sim yet, so it isn't in the integration loop (see Roadmap); its
unit tests cover the same client code paths against fixtures.

### Container path (`make test-compose`)

Brings the sims up as containers (`docker-compose.test.yml`, built from
`Dockerfile.sim`) and runs the `TestCompose*` tests against them over the Docker
network. These skip unless `NIGHTWATCH_SIM_TRUENAS` / `NIGHTWATCH_SIM_AMT` are
set, so `make test-integration` is unaffected.

## Roadmap

Known gaps:

- **Talos gRPC sim.** The full-loop test uses an in-process Talos fake; a real
  mTLS gRPC sim (needs CA + cert machinery) is a follow-up.

Possible additions:

- More BMC drivers behind the same `bmc.type` registry: HPE iLO and other Redfish
  variants, IPMI, plus a Redfish integration sim.
- **Wake-on-LAN** as a power-on path for nodes without a usable BMC.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
