# Images And Releases

CI publishes two images to GHCR on every push to `main` and on version tags:

- `ghcr.io/imlach/nightwatch`: operator, CLI, and `serve` API.
- `ghcr.io/imlach/nightwatch-sim`: protocol simulators for integration tests.

`main` is a rolling tag. Pushing a `vX.Y.Z` git tag publishes immutable
`:vX.Y.Z` images and updates `:latest`.

Consumers should pin by digest:

```text
ghcr.io/imlach/nightwatch:vX.Y.Z@sha256:<digest>
```

CI builds run on GitHub-hosted runners. No hardware or cluster access is
required to build or test the release artifacts.
