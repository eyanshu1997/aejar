export PATH := $(PATH):$(HOME)/go/bin

.PHONY: gen lint test build

# Regenerate Go, grpc-gateway and OpenAPI code from proto/.
gen:
	buf lint
	buf generate

test:
	go vet ./...
	go test ./...

build:
	go build -o bin/ ./cmd/...
