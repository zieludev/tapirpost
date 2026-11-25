VERSION ?= dev

.PHONY: build test check bench bench-http

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o tapirpost ./cmd/tapirpost

# Tests that need PostgreSQL or a Redis-protocol server are skipped unless
# TAPIRPOST_TEST_POSTGRES_URL and TAPIRPOST_TEST_REDIS_URL point at one.
test:
	go test -race ./...

check:
	test -z "$$(gofmt -l .)"
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
	! grep -rnP '[\x{2013}\x{2014}]' --exclude-dir=.git .

bench:
	go test -run '^$$' -bench . -benchtime 2s ./internal/httpapi

bench-http: build
	go run ./tools/bench -binary ./tapirpost
