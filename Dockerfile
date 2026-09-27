FROM golang:1.25-alpine AS build
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
    -ldflags "-X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=${VERSION} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=${COMMIT} -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=${DATE}" \
    -o /out/sorobeacon ./cmd/sorobeacon

FROM alpine:3.20
RUN adduser -D -H sorobeacon && apk add --no-cache ca-certificates
USER sorobeacon
COPY --from=build /out/sorobeacon /usr/local/bin/sorobeacon
EXPOSE 8080
# wget is busybox's, present in the base alpine image; -q -O- discards the
# body and relies on wget's own exit code, which is non-zero on anything
# but a 2xx response.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O- http://localhost:8080/api/v1/health || exit 1
ENTRYPOINT ["sorobeacon"]
