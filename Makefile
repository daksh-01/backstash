# One source of truth for the checks: CI (.github/workflows/ci.yml) and your laptop run the same targets.
MIN_COVER ?= 75
VERSION   ?= dev

.PHONY: check fmt lint test cover integration build

check: fmt lint test ## everything CI runs except integration and the vulnerability scan

fmt:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }

lint:
	golangci-lint run ./...

test:
	go vet ./...
	go test -race ./...

cover: ## fails if total statement coverage drops below MIN_COVER
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total% (minimum $(MIN_COVER)%)"; \
	awk -v t="$$total" -v m="$(MIN_COVER)" 'BEGIN { exit !(t+0 >= m+0) }' || { echo "coverage below $(MIN_COVER)%"; exit 1; }

integration: ## needs the IT_* services; see internal/integration and the CI job
	go test -tags integration -count=1 -v ./internal/integration/...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/backstash ./cmd/backstash
