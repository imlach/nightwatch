# Testing

Nightwatch's tests are designed to exercise real adapter code without requiring
access to real hardware, TrueNAS, Talos, or a Kubernetes cluster.

## Unit Tests

```sh
make test
```

Unit tests are fast and do not require network access. Real adapter clients run
against in-process fakes and golden fixtures:

- AMT uses golden SOAP/XML fixtures for the WS-Man wire format in
  `internal/bmc/amtwsman/testdata`.
- Redfish/iDRAC uses canned iDRAC Redfish responses in
  `internal/bmc/redfish/testdata`, covering power state, reset action, and URL
  handling.

## Integration Tests

```sh
make test-integration
```

This runs:

```sh
go test -tags=integration ./...
```

The real adapter clients talk to protocol simulators in `internal/sim`, so the
actual wire code runs end to end against an in-memory backend. The simulators
bind loopback, require no privileges, and the `integration` build tag keeps this
path out of `make test`.

Covered simulators:

- TrueNAS: a `wss://` JSON-RPC 2.0 server with a self-signed certificate. The
  real client logs in over TLS and its session table feeds the real iSCSI gate.
  Dropping a session makes the gate report clear.
- AMT: a WS-Man HTTP server with digest auth and in-memory power state. The real
  client reads power through Enumerate/Pull and flips state through the digest
  challenge/retry path.
- Full lifecycle: `lifecycle.DrainShutdown` runs end to end against the TrueNAS
  and AMT simulators, with in-process fakes for Kubernetes and Talos.

Redfish has no simulator yet. Its unit tests cover the same client paths against
fixtures.

## Container Path

```sh
make test-compose
```

This brings the simulators up as containers using `docker-compose.test.yml`,
built from `Dockerfile.sim`, then runs the `TestCompose*` tests against them over
the Docker network.

The compose tests skip unless `NIGHTWATCH_SIM_TRUENAS` and
`NIGHTWATCH_SIM_AMT` are set, so `make test-integration` is unaffected.
