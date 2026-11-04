FROM golang:1.27-alpine AS build
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build info baked in via ldflags, so /api/v1/version identifies the
# exact image a running container came from.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=${VERSION} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=${COMMIT} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=${DATE}" \
    -o /out/sorobeacon ./cmd/sorobeacon \
    && printf 'sorobeacon:x:10001:10001:SoroBeacon:/nonexistent:/sbin/nologin\n' > /out/passwd \
    && printf 'sorobeacon:x:10001:\n' > /out/group \
    && mkdir -p -m 0750 /out/tmp

# Hardened runtime stage: FROM scratch carries only the statically linked
# binary, root CA bundle, timezone database, and minimal non-root passwd/group
# entries. No shell (/bin/sh), no package manager (apk), and no external
# network utilities (wget, curl).
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group /etc/group
COPY --from=build --chown=10001:10001 /out/tmp /tmp
COPY --from=build /out/sorobeacon /usr/local/bin/sorobeacon
# Numeric UID/GID so Kubernetes can enforce runAsNonRoot without resolving a
# username inside the container. Matches deploy/helm/sorobeacon/values.yaml.
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/sorobeacon", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/sorobeacon"]
