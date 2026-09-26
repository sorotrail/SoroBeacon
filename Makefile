.PHONY: build run test test-db cover lint fmt up down clean migrate-new bench

MIGRATIONS_DIR := internal/store/migrations
# SQLite keeps its own DDL at the same version numbers; migrate-new scaffolds
# both so the two sets cannot drift apart.
SQLITE_MIGRATIONS_DIR := internal/store/migrations/sqlite

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

build:
	go build -ldflags "-X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=$(VERSION) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=$(COMMIT) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=$(DATE)" -o bin/sorobeacon ./cmd/sorobeacon

run: build
	./bin/sorobeacon

test:
	go test ./...

# Rule-evaluation hot path. Does not run as part of `make test` / `go test ./...`.
bench:
	go test -bench=. -benchmem ./internal/rules/...

# Run all tests including the store integration tests, against the
# docker-compose Postgres (make up first, or any Postgres you point at).
test-db:
	TEST_DATABASE_URL=$${TEST_DATABASE_URL:-postgres://sorobeacon:sorobeacon@localhost:5432/sorobeacon?sslmode=disable} go test ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

# make migrate-new name=add_foo scaffolds NNNN_add_foo.{up,down}.sql in both
# internal/store/migrations (Postgres) and internal/store/migrations/sqlite,
# NNNN one past the highest existing Postgres pair. The next number is computed
# with awk so a zero-padded value like 0009 is not misread as octal and the
# recipe stays POSIX-sh portable. A migration that needs no SQLite change keeps
# an empty (or comment-only) pair, as 0005_channels_config_encryption_comment
# does.
migrate-new:
ifndef name
	$(error usage: make migrate-new name=<snake_case_name>)
endif
	@next=$$(ls $(MIGRATIONS_DIR)/*.up.sql 2>/dev/null | sed -E 's#.*/([0-9]+)_.*#\1#' | sort -n | tail -1 \
		| awk 'BEGIN{n=0} {n=$$1+0} END{printf "%04d", n+1}'); \
	for dir in $(MIGRATIONS_DIR) $(SQLITE_MIGRATIONS_DIR); do \
		up=$$dir/$${next}_$(name).up.sql; \
		down=$$dir/$${next}_$(name).down.sql; \
		touch "$$up" "$$down"; \
		echo "created $$up"; \
		echo "created $$down"; \
	done

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
