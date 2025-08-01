# Copyright 2025 Robert Bosch GmbH
#
# SPDX-License-Identifier: Apache-2.0


PACKAGE          := $(shell basename $(shell pwd))
SRC           	 := $(shell find . -type f -name '*.go' -print)
BIN_DIR       	 := $(CURDIR)/bin
CLI_TOOLS    	 ?= $(BIN_DIR)/$(PACKAGE)
DOCKER_TOOLS	 = report
GOFLAGS       	 :=
GOPRIVATE     	 ?= github.com/boschglobal


.PHONY: all
all: build

.PHONY: build
build: cmd

cmd: $(CLI_TOOLS)

$(CLI_TOOLS): $(SRC)
	go build $(GOFLAGS) -o $@ ./cmd/$(subst $(BIN_DIR)/,,$@)

.PHONY: install
install: build
	ln -sf $(BIN_DIR)/$(PACKAGE) ~/.local/bin/dse-$(PACKAGE)
	mkdir -p ~/.local/share/dse-$(PACKAGE)
	ln -sf $(CURDIR)/cmd/$(PACKAGE)/reports ~/.local/share/dse-$(PACKAGE)/

.PHONY: docker
docker:
	for d in $(DOCKER_TOOLS) ;\
	do \
		docker build -f build/package/$$d/Dockerfile \
				--tag dse-$$d:test . ;\
	done;

.PHONY: graph
graph:
ifeq ($(shell docker container list -q -f name=memgraph 2> /dev/null),)
	docker run -d --rm --name memgraph \
		-p 3000:3000 \
		-p 7444:7444 \
		-p 7687:7687 \
		-v mg_lib:/var/lib/memgraph \
		memgraph/memgraph-platform
endif

.PHONY: test
test:
	go test ./...
	go test ./cmd/... -tags test_e2e
	go test ./internal/... -tags test_e2e

.PHONY: format
format:
	go fmt -x ./...

.PHONY: lint
lint:
	go fmt -x ./...
	docker run -t --rm \
		-v $$(pwd):/app \
		-v ~/.cache/golangci-lint/1.59.1:/root/.cache \
		-w /app \
		golangci/golangci-lint:latest \
		golangci-lint run -v ./...


.PHONY: clean
clean:
	@rm -rf $(BIN_DIR)

.PHONY: cleanall
cleanall:
