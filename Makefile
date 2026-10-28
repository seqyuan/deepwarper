BINARY := dw
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet fmt doctor-local

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/dw

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

doctor-local: build
	./$(BINARY) doctor -c examples/config.local.yaml
