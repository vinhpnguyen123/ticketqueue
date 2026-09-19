# syntax=docker/dockerfile:1
#
# worker — multi-stage: the toolchain never reaches the runtime image.
#
# golang:1.26-alpine, not 1.22: go.mod declares go 1.26.3 and an older
# toolchain refuses the module outright.

FROM golang:1.26-alpine AS build
WORKDIR /src

# go.mod/go.sum first, on their own layer: dependencies change far less often
# than source, so this download is cached across almost every rebuild.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# ARG here rather than at the top: an ARG invalidates every layer after it, so
# a new SHA must not invalidate the dependency download above.
ARG GIT_SHA=dev
# CGO_ENABLED=0 produces a static binary, which is what lets the runtime image
# be distroless/static. -s -w drops the symbol table and DWARF data (~30%).
RUN CGO_ENABLED=0 go build \
      -ldflags="-s -w -X github.com/vinhpnguyen123/ticketqueue/internal/version.SHA=${GIT_SHA}" \
      -o /out/worker ./cmd/worker

# distroless/static, not alpine: no shell, no package manager, no busybox, so
# the attack surface is the binary and the CA bundle. The cost is that a
# HEALTHCHECK has nothing to exec — which is why each binary carries its own
# -healthcheck probe instead of the image carrying curl. alpine would have
# given a shell for debugging and cost ~6MB more; `docker logs` and the
# /health endpoint have been enough.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/worker /worker
# nonroot (uid 65532) is baked into the tag; named here so it is visible.
USER nonroot:nonroot

# The worker is not an HTTP server; this port exists for /metrics, and
# /health was added beside it so the container has something to answer.
EXPOSE 9091
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/worker", "-healthcheck", "http://127.0.0.1:9091/health"]

ENTRYPOINT ["/worker"]
