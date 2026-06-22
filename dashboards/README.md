# Dashboards

`nightwatch-operator.json` — a portable Grafana dashboard for the operator's
**own** emitted metrics: the standard controller-runtime + Go runtime series on
the manager's `/metrics` endpoint (`--metrics-bind-address`, default `:8080`).
The operator exposes no custom metrics, so this covers reconcile rate/latency,
reconcile errors, workqueue depth/retries, leader-election status, Kubernetes
API client request codes, and process/runtime health.

It is **cluster-agnostic**: two template variables (`datasource`, `job`) bind it
to any Prometheus and any scrape job — there are no hardcoded endpoints,
namespaces, or cluster labels. ElasticNode power-state, drain/wake activity, and
anything else from the *target* cluster are deliberately out of scope here (they
come from kube-state-metrics custom-resource-state and other cluster-specific
sources, not the operator's metrics).

## Use

- **Grafana UI**: Dashboards → Import → upload the JSON, pick your Prometheus.
- **grafana-operator**: reference the JSON in a `GrafanaDashboard` CR (`spec.url`
  pointing at the raw file, or inlined `spec.json`).
- **Provisioning sidecar**: drop it in a dashboards ConfigMap/folder.

Scrape the operator with a ServiceMonitor/PodMonitor on the metrics port; the
`job` variable then lists the available jobs. Requires controller-runtime
metrics (this build uses v0.24.x: `controller_runtime_reconcile_time_seconds`,
workqueue metrics labelled by `controller`).
