GO ?= ./scripts/go

.PHONY: setup build test race vet fmt check demo clean
setup:
	./scripts/bootstrap-go
build:
	$(GO) build -o bin/airlock ./cmd/airlock
test:
	$(GO) test ./...
race:
	$(GO) test -race ./...
vet:
	$(GO) vet ./...
fmt:
	$(GO) fmt ./...
check: test race vet
	@test -z "$$($(GO) fmt ./...)" || (echo 'Go formatting changed files; review and rerun make check'; exit 1)
demo: build
	./bin/airlock demo
clean:
	rm -rf bin
