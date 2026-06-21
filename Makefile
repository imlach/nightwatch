.PHONY: test test-integration test-compose test-docker fmt-docker

GO_IMAGE ?= golang:1.26

# Unit tests (fast, no network/containers).
test:
	go test ./...

# Integration tests: drive the real truenas/amtwsman clients at in-process
# protocol sims (internal/sim). Behind the `integration` build tag so `make test`
# is unaffected. This is the CI entry point for the hardware-free path.
test-integration:
	go test -tags=integration ./...

# Container path: bring the sims up as containers and run the env-driven compose
# tests against them. Exits non-zero if the test runner fails.
test-compose:
	docker compose -f docker-compose.test.yml up --build \
		--abort-on-container-exit --exit-code-from tests
	docker compose -f docker-compose.test.yml down -v

test-docker:
	docker run --rm -v "$$(pwd):/src" -w /src $(GO_IMAGE) go test ./...

fmt-docker:
	docker run --rm -v "$$(pwd):/src" -w /src $(GO_IMAGE) gofmt -w $$(find . -name '*.go')
