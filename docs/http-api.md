# HTTP Trigger API

`nightwatch serve` exposes the same drain-shutdown, wake, and status paths used
by the CLI through the shared `internal/operate` layer. It is intended for a
remote scheduler, CronJob, or small automation service.

## Startup

- `--listen` controls the bind address.
- `NIGHTWATCH_LISTEN` is the equivalent environment variable.
- The default listen address is `:8080`.

Auth fails closed. `NIGHTWATCH_API_TOKEN` must be set or `serve` refuses to
start. Every `/v1/*` request must include:

```http
Authorization: Bearer <token>
```

`/healthz` is open.

## Endpoints

| Method and path | Body | Result |
| --- | --- | --- |
| `POST /v1/nodes/{node}/drain-shutdown` | `{forceBmcOff?, dryRun?}` | `200 {ok,node,steps[]}` |
| `POST /v1/nodes/{node}/wake` | `{skipGpuWait?, dryRun?}` | `200 {ok,node,steps[]}` |
| `GET /v1/nodes/{node}/status` | none | `{node,bmcPower,reachable?}` |
| `GET /healthz` | none | `200` |

Common errors:

- `404`: unknown node.
- `409`: operation stopped.
- `500`: backend assembly or lifecycle failure.

One request targets one named node. There is no broadcast endpoint.

## Runtime Behavior

Lifecycle operations run synchronously inside the request. A drain can take
minutes, so the server write timeout is intentionally generous and callers must
set a matching long client timeout.

SIGINT and SIGTERM drain in-flight requests, then the server exits.
