.PHONY: build build-tf run test test-db test-race vulncheck cover lint fmt up down clean migrate-new bench verify-reproducible

MIGRATIONS_DIR := internal/store/migrations
# SQLite keeps its own DDL at the same version numbers; migrate-new scaffolds
# both so the two sets cannot drift apart.
SQLITE_MIGRATIONS_DIR := internal/store/migrations/sqlite

# The Terraform provider is a nested module. Every target below cds into it
# because the root module's ./... deliberately stops at a subdirectory that
# carries its own go.mod, so a recipe written like the ones above would build
# nothing and say so.
TF_DIR := terraform-provider-sorobeacon

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo none)
# Derive DATE deterministically from the commit timestamp (or SOURCE_DATE_EPOCH
# when set) rather than the wall clock (`date -u`) so two builds of the same
# commit on different hosts or at different times produce byte-for-byte
# identical binaries while still stamping /api/v1/version.
SOURCE_DATE_EPOCH ?= $(shell git show -s --format=%ct HEAD 2>/dev/null || echo 0)
DATE ?= $(shell TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD 2>/dev/null || echo 1970-01-01T00:00:00Z)

GO_BUILD_FLAGS ?= -trimpath -buildvcs=false
LDFLAGS ?= -s -w -buildid= -X github.com/sorotrail/sorobeacon/internal/buildinfo.Version=$(VERSION) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Commit=$(COMMIT) -X github.com/sorotrail/sorobeacon/internal/buildinfo.Date=$(DATE)
TF_LDFLAGS ?= -s -w -buildid=

build:
	CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(LDFLAGS)" -o bin/sorobeacon ./cmd/sorobeacon

verify-reproducible:
	./scripts/verify-reproducible-build.sh

run: build
	./bin/sorobeacon

test:
	go test ./...

# The race detector is roughly two to three times slower than a plain run, so
# it is not part of `make test`. CI runs this target in its own job; run it
# locally when touching anything the poller, dispatcher or HTTP servers share.
test-race:
	go test -race ./...

# Pinned rather than tracking latest: an unpinned scanner turns an unrelated
# pull request red on the day the vulnerability database updates. Run through
# `go run` so the tool stays out of the module graph and does not cause
# `go mod tidy` churn. The Terraform provider is a separate module, so a scan
# of the root module alone would give false assurance.
#
# The scan runs on a patched toolchain even though the module's go directive
# is the minimum the project builds with: the standard library is part of the
# attack surface, and 1.26.0 carries advisories that 1.27.3 fixes. Builds and
# tests stay on whatever `go-version-file: go.mod` installs; only the scanner
# is pinned, so its answer does not change with the reviewer's local Go.
VULNCHECK_VERSION ?= v1.9.0
VULNCHECK_TOOLCHAIN ?= go1.27.3
vulncheck:
	GOTOOLCHAIN=$(VULNCHECK_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@$(VULNCHECK_VERSION) ./...
	cd terraform-provider-sorobeacon && GOTOOLCHAIN=$(VULNCHECK_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@$(VULNCHECK_VERSION) ./...

# Rule-evaluation and poller capacity hot paths. Does not run as part of `make test` / `go test ./...`.
bench:
	go test -bench=. -benchmem ./internal/rules/... ./internal/poller/...

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

# Mirrors the terraform-provider job in .github/workflows/ci.yml, so the CI
# result is reproducible on a laptop. The binary lands beside the server's in
# bin/ rather than next to the provider source.
build-tf:
	cd $(TF_DIR) && CGO_ENABLED=0 go build $(GO_BUILD_FLAGS) -ldflags "$(TF_LDFLAGS)" -o ../bin/terraform-provider-sorobeacon .

test-tf:
	cd $(TF_DIR) && go test ./... -count=1

lint-tf:
	cd $(TF_DIR) && golangci-lint run

tidy-tf:
	cd $(TF_DIR) && go mod tidy

up:
	docker compose up --build -d

down:
	docker compose down

clean:
	rm -rf bin
