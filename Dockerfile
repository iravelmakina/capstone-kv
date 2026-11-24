# capstone-kv node image (ADR-0009): one static Go binary, both roles.
# Built in CI, saved with `docker save`, shipped through S3 and loaded on EC2.

# --- build stage: compile with the Go toolchain -----------------------------------------
# The build stage runs on the machine's own CPU ($BUILDPLATFORM, e.g. an ARM Mac) and
# cross-compiles for the target ($TARGETARCH, amd64 for t3 instances): no slow emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
# Dependencies first, so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# CGO off = a fully static binary that needs no C library in the final image.
# -trimpath and -s -w drop local paths and debug symbols: smaller, reproducible.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/kvnode ./cmd/kvnode
# An empty data directory to copy into the final image (it has no shell to run mkdir).
RUN mkdir -p /out/data

# --- run stage: nothing but the binary ---------------------------------------------------
# distroless/static has no shell or package manager (less to attack) and a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kvnode /kvnode
# The append-only logs live here. It must belong to the non-root user, or the program
# can't create its files: a new named volume copies this ownership; on EC2 the host
# directory mounted here is chown'ed to the same UID (65532) by the instance's user data.
COPY --from=build --chown=nonroot:nonroot /out/data /data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/kvnode"]
