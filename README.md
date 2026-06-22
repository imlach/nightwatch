# Nightwatch

Nightwatch powers bare-metal worker nodes down when they are not needed, then
brings them back cleanly when capacity returns.

It is built for Talos-based Kubernetes clusters where the hard part is not
turning a machine off. The hard part is doing it without corrupting storage,
stranding workloads, or needing a human to remember which out-of-band controller
each box uses.

Nightwatch coordinates the full lifecycle:

1. Cordon and drain the Kubernetes node.
2. Wait for iSCSI sessions to clear from TrueNAS.
3. Ask Talos to shut the OS down cleanly.
4. Power the hardware off through its BMC.
5. Wake it later and wait for Kubernetes and GPUs to come back.

This repository is a personal infrastructure tool published in the open for
transparency and reuse. It is not packaged as a supported OSS product, but the
core pieces are documented and tested without requiring access to the author's
hardware.

## What It Talks To

Nightwatch is intentionally small, but it crosses several control planes:

- Kubernetes, for node drain/uncordon and the `ElasticNode` operator API.
- Talos, for clean OS shutdown and machine readiness.
- TrueNAS, for iSCSI session checks before power-off.
- Intel AMT, via WS-Man over HTTP digest.
- Dell iDRAC9 and compatible Redfish controllers, via HTTPS.

The BMC layer is selected per node from inventory (`bmc.type: amt`, `redfish`,
or `idrac`), so mixed hardware can share one workflow.

## Ways To Run It

- **Operator**: the primary workflow. A management cluster runs
  `nightwatch-operator`, which reconciles `ElasticNode` resources toward
  `spec.desiredPower: On|Off`.
- **CLI**: direct `drain-shutdown` and `wake` commands for manual operation or
  testing.
- **HTTP API**: `nightwatch serve` exposes the same lifecycle path behind a
  bearer token for schedulers or CronJobs.

The lifecycle code is shared across all three entry points, so the operator,
CLI, and API converge through the same safety gates.

## Quick Orientation

```yaml
nodes:
  node-1:
    elastic_eligible: true
    talos_endpoint: "192.0.2.10"
    iscsi_initiator_addr: "192.0.2.10"
    kube_node_name: node-1
    bmc:
      type: amt
      host: "192.0.2.1"
    gpus:
      - example-gpu
```

See [examples/nodes.yml](examples/nodes.yml) for a fuller inventory example.

Manifests for the operator CRD and RBAC live in [config/](config/). The operator
serves standard controller-runtime and Go runtime metrics, and a portable
Grafana dashboard is available in [dashboards/](dashboards/).

## Documentation

- [Operator workflow](docs/operator.md): CRD behavior, management-vs-target
  cluster wiring, inventory, credentials, BMC drivers, and observability.
- [HTTP trigger API](docs/http-api.md): endpoints, auth, request behavior, and
  operational notes.
- [Images and releases](docs/releases.md): published images, tag semantics, and
  digest pinning.
- [Testing](docs/testing.md): unit tests, protocol simulators, integration tests,
  and the Docker Compose path.
- [Dashboards](dashboards/README.md): importing and wiring the portable Grafana
  dashboard.

## Development

```sh
make test
make test-integration
```

Both paths run without real hardware. Integration tests drive the real adapter
clients against in-process protocol simulators.

## Roadmap

Known gaps:

- Talos gRPC simulator. The full-loop test currently uses an in-process Talos
  fake; a real mTLS gRPC simulator needs CA and certificate machinery.
- Redfish integration simulator. Redfish/iDRAC is covered by fixture-backed unit
  tests today.

Possible additions:

- More BMC drivers behind the same `bmc.type` registry, such as HPE iLO, IPMI,
  and other Redfish variants.
- Wake-on-LAN as a power-on path for nodes without a usable BMC.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
