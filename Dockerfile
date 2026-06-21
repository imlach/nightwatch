# Nightwatch - bare-metal node lifecycle orchestrator.
# Build context is the repo root.

FROM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -o /out/nightwatch ./cmd/nightwatch && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -o /out/nightwatch-operator ./cmd/nightwatch-operator

FROM alpine:3.24
# Static CGO_ENABLED=0 binary needs only the CA bundle for outbound TLS
# (Redfish/Talos/k8s API). Copy it from the builder rather than `apk add`.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/nightwatch /usr/local/bin/nightwatch
COPY --from=builder /out/nightwatch-operator /usr/local/bin/nightwatch-operator
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nightwatch"]
