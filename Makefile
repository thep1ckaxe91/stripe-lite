SHELL := /bin/bash
BIN_DIR := $(CURDIR)/bin
DIST_DIR := $(CURDIR)/dist
OAPI_CODEGEN := $(BIN_DIR)/oapi-codegen
BUNDLED_SPEC := $(DIST_DIR)/openapi.bundled.yaml

.PHONY: all build run test test-integration test-intergration lint-spec lint-redocly bundle-spec codegen clean verify tools

all: lint-spec lint-redocly codegen test build

tools: $(OAPI_CODEGEN)

$(OAPI_CODEGEN):
	@mkdir -p $(BIN_DIR)
	GOBIN=$(BIN_DIR) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1

bundle-spec:
	@mkdir -p $(DIST_DIR)
	@npx --yes @redocly/cli@2.60.0 bundle api/openapi.yaml -o $(BUNDLED_SPEC)
	@echo "==> Bundled OpenAPI spec at $(BUNDLED_SPEC)"

codegen: $(OAPI_CODEGEN) bundle-spec
	@mkdir -p internal/api/v1
	$(OAPI_CODEGEN) -config oapi-codegen.yaml $(BUNDLED_SPEC)
	@echo "==> Code generation complete: internal/api/v1/v1.gen.go"

lint-spec:
	@go run ./cmd/lint-spec api/openapi.yaml

lint-redocly:
	@npx --yes @redocly/cli@2.60.0 lint api/openapi.yaml

build: codegen
	@mkdir -p $(BIN_DIR)
	go build -ldflags="-s -w" -o $(BIN_DIR)/server ./cmd/server
	@echo "==> Binary built at $(BIN_DIR)/server"

run: build
	$(BIN_DIR)/server

test: codegen
	go test -v -race ./...

test-integration: codegen
	./scripts/with-test-db.sh go test -tags=integration -v -race ./...

test-intergration: test-integration

verify: build
	@chmod +x scripts/verify.sh
	./scripts/verify.sh

clean:
	rm -rf $(BIN_DIR) $(DIST_DIR) internal/api/v1/*.gen.go
