APP := secretmediabot
GO ?= go
GOOSE_VERSION ?= v3.27.3
GOOSE := $(GO) run github.com/pressly/goose/v3/cmd/goose@$(GOOSE_VERSION)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATABASE_DRIVER ?= postgres

-include .env

.PHONY: all build run test test-race vet fmt fmt-check check clean \
	compose-up compose-down compose-logs migrate-up migrate-down migrate-status

all: check build

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath \
		-ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)" \
		-o bin/$(APP) ./cmd/bot

run:
	$(GO) run ./cmd/bot

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './vendor/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -type f -name '*.go' -not -path './vendor/*'))" || \
		{ echo "Go files need formatting; run 'make fmt'"; exit 1; }

check: fmt-check vet test

clean:
	rm -rf ./bin ./coverage.out

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down

compose-logs:
	docker compose logs -f bot postgres

migrate-up:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is required"; exit 1; }
	$(GOOSE) -dir migrations $(DATABASE_DRIVER) "$(DATABASE_URL)" up

migrate-down:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is required"; exit 1; }
	$(GOOSE) -dir migrations $(DATABASE_DRIVER) "$(DATABASE_URL)" down

migrate-status:
	@test -n "$(DATABASE_URL)" || { echo "DATABASE_URL is required"; exit 1; }
	$(GOOSE) -dir migrations $(DATABASE_DRIVER) "$(DATABASE_URL)" status
