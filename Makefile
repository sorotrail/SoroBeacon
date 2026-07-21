.PHONY: build run test test-db lint fmt up down clean

build:
	go build -o bin/sorobeacon ./cmd/sorobeacon

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
