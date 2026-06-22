# Operator Workflow

`cmd/nightwatch-operator` is the primary Nightwatch entry point. It is a
leader-elected controller that runs on a management cluster and reconciles
`ElasticNode` resources toward `spec.desiredPower`.

`ElasticNode` resources live in the management cluster. The nodes being powered
up and down can live in a separate target cluster.

## Control Model

`ElasticNode` is an intent record:

- `spec.desiredPower: On|Off` is the desired hardware state. The default is
  `On`, which is the fail-safe in-service state.
- `spec.clusterRef` identifies the target cluster. Empty selects the default
  target cluster.
- `status` is observability only. The controller never uses status as input for
  the next decision.

Each reconcile loop reads live state from the backing systems:

- BMC power state.
- Kubernetes node readiness from the target cluster.
- GPU visibility from the target cluster.
- iSCSI session state from TrueNAS.
- Talos machine reachability/readiness.

It then drives one of the lifecycle state machines:

- `lifecycle.DrainShutdown` for `desiredPower: Off`.
- `lifecycle.PowerOn` for `desiredPower: On`.

## Management And Target Clients

The operator uses two Kubernetes clients:

- The management-cluster client watches and updates `ElasticNode` resources.
- The target-cluster client performs cordon, drain, uncordon, Ready checks, and
  GPU checks against the workload cluster being scaled.

The cross-cluster `Backends` provider in
`internal/controller/provider.go` builds the target-cluster client and per-node
BMC, Talos, and iSCSI adapters from:

- the inventory ConfigMap;
- the credentials Secret;
- the target kubeconfig and talosconfig.

CRD and RBAC manifests live in [../config/](../config/).

## Inventory

The inventory maps a Kubernetes node to its Talos endpoint, iSCSI identity, BMC,
and optional metadata:

```yaml
nodes:
  node-1:
    elastic_eligible: true
    role: gpu-worker-small
    talos_endpoint: "192.0.2.10"
    iscsi_initiator_addr: "192.0.2.10"
    kube_node_name: node-1
    bmc:
      type: amt
      host: "192.0.2.1"
    gpus:
      - example-gpu
```

`iscsi_initiator_addr` is the TrueNAS session identity. It is deliberately
separate from `talos_endpoint`, even when both happen to have the same address.

See [../examples/nodes.yml](../examples/nodes.yml).

## BMC Drivers

Drivers self-register by `bmc.type`:

- `amt`: Intel AMT through `internal/bmc/amtwsman`.
- `redfish` or `idrac`: Redfish through `internal/bmc/redfish`.

The Redfish driver uses HTTP Basic auth and skips TLS verification for iDRAC
self-signed certificates.

## Observability

The operator serves standard controller-runtime and Go runtime metrics on
`--metrics-bind-address` (default `:8080`). It currently registers no custom
metrics.

A portable Grafana dashboard for those process/controller metrics is in
[../dashboards/](../dashboards/). It binds to Prometheus through template
variables and carries no cluster-specific labels.

ElasticNode power state, drain/wake activity, and other target-cluster signals
should come from cluster-specific sources such as kube-state-metrics
custom-resource-state.
