# The ops agent's image (ADR-0034). Built from the repository root:
#   docker build -f cmd/kvops/Dockerfile -t kvops:local .
# It runs with only what a run needs (make ops-agent): the Anthropic key, a status-scoped
# store key, its proposals/ and runs/ directories. No AWS credentials, no admin key, no
# approvals directory: whatever the model is told, this process can't act.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY cmd/kvops ./cmd/kvops
WORKDIR /src/cmd/kvops
RUN go mod download
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/kvops .

# distroless: no shell, CA certificates for api.anthropic.com, a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kvops /kvops
COPY cmd/kvops/testdata /testdata
USER nonroot:nonroot
ENTRYPOINT ["/kvops"]
