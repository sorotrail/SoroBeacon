FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/sorobeacon ./cmd/sorobeacon

FROM alpine:3.20
RUN adduser -D -H sorobeacon && apk add --no-cache ca-certificates
USER sorobeacon
COPY --from=build /out/sorobeacon /usr/local/bin/sorobeacon
EXPOSE 8080
ENTRYPOINT ["sorobeacon"]
