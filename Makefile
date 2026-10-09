GO ?= ./scripts/go

.PHONY: setup build test race vet fmt check stress demo clean
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
	@test -z "$$("$$($(GO) env GOROOT)/bin/gofmt" -l cmd internal)" || (echo 'Go files need formatting; run make fmt'; exit 1)
stress:
	$(GO) test -v ./internal/reliability -run '^TestStress$$' -count=1
demo: build
	./bin/airlock demo
clean:
	rm -rf bin
