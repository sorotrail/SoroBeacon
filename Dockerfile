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
ENTRYPOINT ["sorobeacon"]
