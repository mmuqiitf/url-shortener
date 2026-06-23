help:
	@echo "Targets:"
	@echo "  make build           - build the server binary into ./bin/server"
	@echo "  make run             - build and run locally"
	@echo "  make test            - run all tests"
	@echo "  make test-race       - run tests with the data-race detector (requires CGO/gcc)"
	@echo "  make bench           - run benchmarks"
	@echo "  make lint            - run golangci-lint (install separately if missing)"
	@echo "  make fmt             - run gofmt + goimports"
	@echo "  make tidy            - go mod tidy"
	@echo "  make smoke           - run the curl-based smoke test (server must be running)"
	@echo "  make docker-build    - build the Docker image"
	@echo "  make docker-up       - docker compose up -d"
	@echo "  make docker-down     - docker compose down"
	@echo "  make docker-logs     - follow container logs"

build:
	go build -trimpath -ldflags="-s -w" -o bin/server ./cmd/server

run: build
	./bin/server

test:
	go test ./...

test-race:
	CGO_ENABLED=1 go test -race ./...

bench:
	go test -bench=. -benchmem ./...

fmt:
	gofmt -w .
	goimports -w .

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

smoke:
	bash scripts/smoke.sh

docker-build:
	docker build -t url-shortener:local .

docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f --tail=100
