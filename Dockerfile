# Container image for vianden-server.
#
# Two stages: the first builds the program with the full Go toolchain; the second
# contains ONLY the finished program and the license notices. No shell, no package
# manager, no OS files: there is almost nothing in the image an attacker could use.
#
# Build:  docker build -t vianden-server .
# (Hosts normally use deploy/docker-compose.yml, which builds and runs it.)

# ---- stage 1: build ----
FROM golang:1.27-alpine AS build
WORKDIR /src

# Download dependencies first: Docker caches this step until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION (optional) overrides the version written in internal/buildinfo.
ARG VERSION=
# CGO_ENABLED=0: a fully static program that needs no system libraries (required for "scratch").
# -trimpath: no local folder names in the binary. -s -w: smaller binary (no debug symbols).
RUN set -e; \
    LDFLAGS="-s -w"; \
    if [ -n "$VERSION" ]; then LDFLAGS="$LDFLAGS -X github.com/5cfp/vianden-server/internal/buildinfo.Version=$VERSION"; fi; \
    CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o /out/vianden-server ./cmd/server

# Third-party license notices (MIT/BSD/Apache require shipping them with the program),
# plus the Go standard library's license (it is compiled into the program too).
RUN go install github.com/google/go-licenses/v2@v2.0.1 \
 && go-licenses save ./cmd/server --save_path=/out/licenses --ignore github.com/5cfp/vianden-server \
 && mkdir -p /out/licenses/vianden-server /out/licenses/go \
 && cp LICENSE /out/licenses/vianden-server/LICENSE \
 && cp "$(go env GOROOT)/LICENSE" /out/licenses/go/LICENSE

# The data folder must exist and belong to the unprivileged user (scratch has no mkdir).
RUN mkdir -p /out/data

# ---- stage 2: the image that runs ----
FROM scratch
COPY --from=build /out/vianden-server /vianden-server
COPY --from=build /out/licenses /licenses
COPY --from=build --chown=65532:65532 /out/data /data

# Run as an unprivileged user (not root). 65532 is the conventional "nonroot" id.
USER 65532:65532

# Defaults for running inside a container (overridden by docker-compose or -e).
# Ports above 1024 work without root; the host maps 443 -> 8443 and 80 -> 8080.
ENV VIANDEN_DATA_DIR=/data \
    VIANDEN_LISTEN_ADDR=:8080 \
    VIANDEN_HTTPS_ADDR=:8443 \
    VIANDEN_HTTP_ADDR=:8080

VOLUME ["/data"]
EXPOSE 8080 8443 50000/udp

# Docker marks the container unhealthy if the server stops answering.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["/vianden-server", "healthcheck"]

ENTRYPOINT ["/vianden-server"]
