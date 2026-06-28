# Dashboards

`nightwatch-operator.json` is a portable Grafana dashboard for the operator's
own `/metrics` endpoint (`--metrics-bind-address`, default `:8080`). It uses the
standard controller-runtime, client-go, process, and Go runtime metrics that the
manager already exports; the operator does not currently register custom
Nightwatch metrics.

The dashboard is cluster-agnostic. It has template variables for `datasource`,
`job`, `namespace`, `pod`, and `controller`, and it does not hardcode endpoints,
node names, namespaces, cluster labels, scrape jobs, or deployment-specific
topology. The default controller value is `elasticnode`, which is the
Nightwatch controller name rather than an environment-specific label.

The panels focus on signals that are useful while operating the controller:
scrape health, leader election, reconcile error rate and latency, workqueue
depth/retries/stuck work, active worker saturation, Kubernetes API request
errors, CPU, memory, goroutines, and GC pause time.

## Use

- **Grafana UI**: Dashboards → Import → upload the JSON, pick your Prometheus.
- **grafana-operator**: reference the JSON in a `GrafanaDashboard` CR (`spec.url`
  pointing at the raw file, or inlined `spec.json`).
- **Provisioning sidecar**: drop it in a dashboards ConfigMap/folder.

Scrape the operator with a ServiceMonitor, PodMonitor, or equivalent Prometheus
scrape config. Pick the matching `job` after import, then narrow by `namespace`
and `pod` if those labels are present in your Prometheus setup. Leave those
variables on `All` for scrape configurations that do not attach Kubernetes
metadata labels.

Expected metric families:

- `controller_runtime_reconcile_total`
- `controller_runtime_reconcile_errors_total`
- `controller_runtime_reconcile_panics_total`
- `controller_runtime_terminal_reconcile_errors_total`
- `controller_runtime_reconcile_timeouts_total`
- `controller_runtime_reconcile_time_seconds_bucket`
- `controller_runtime_active_workers`
- `controller_runtime_max_concurrent_reconciles`
- `workqueue_*`
- `rest_client_requests_total`
- `process_*`
- `go_*`
