.PHONY: build run test test-db cover lint fmt up down clean migrate-new

MIGRATIONS_DIR := internal/store/migrations

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

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# make migrate-new name=add_foo scaffolds internal/store/migrations/NNNN_add_foo.{up,down}.sql,
# NNNN one past the highest existing pair. 10# forces base-10 arithmetic so
# a zero-padded number like 0009 isn't misread as octal.
migrate-new:
ifndef name
	$(error usage: make migrate-new name=<snake_case_name>)
endif
	@last=$$(ls $(MIGRATIONS_DIR)/*.up.sql 2>/dev/null | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | tail -1); \
	next=$$(printf '%04d' $$((10#$${last:-0} + 1))); \
	up=$(MIGRATIONS_DIR)/$${next}_$(name).up.sql; \
	down=$(MIGRATIONS_DIR)/$${next}_$(name).down.sql; \
	touch "$$up" "$$down"; \
	echo "created $$up"; \
	echo "created $$down"

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
