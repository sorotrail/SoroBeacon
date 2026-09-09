.PHONY: build run test test-db lint fmt up down clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

build:
	go build -ldflags "-X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=$(VERSION) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=$(COMMIT) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=$(DATE)" -o bin/sorobeacon ./cmd/sorobeacon

run: build
	./bin/sorobeacon

test:
	go test ./...

# Run all tests including the store integration tests, against the
# docker-compose Postgres (make up first, or any Postgres you point at).
test-db:
	TEST_DATABASE_URL=$${TEST_DATABASE_URL:-postgres://sorobeacon:sorobeacon@localhost:5432/sorobeacon?sslmode=disable} go test ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

up:
	docker compose up --build -d

down:
	docker compose down

clean:
	rm -rf bin
