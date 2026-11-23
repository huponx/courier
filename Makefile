GO      ?= go
BIN     ?= bin
COMPOSE ?= docker compose
IMAGE_TAG ?= dev
IMAGE_REPO ?= ghcr.io/huponx

.PHONY: help tidy upgrade outdated test vet build docker docker-courier docker-worker \
	courier worker courierctl \
	run-courier run-worker send \
	redis-up redis-down redis-logs \
	generate clean

help:
	@printf '%s\n' \
		'tidy          go mod tidy' \
		'upgrade  upgrade the dependencies (PATCH=1: patch releases only), tidy, build and test' \
		'outdated  list the dependencies that have a newer version' \
		'test          go test ./...' \
		'vet           go vet ./...' \
		'build         build courier, worker, courierctl into $(BIN)/' \
		'run-courier   go run ./cmd/courier' \
		'run-worker    go run ./cmd/worker' \
		'send          courierctl (override with ARGS=...)' \
		'redis-up      docker compose up -d' \
		'redis-down    docker compose down' \
		'docker        build both images (IMAGE_TAG=dev)' \
		'generate      buf generate Go stubs (PROTO_REF=vX.Y.Z; needs go.work, see README)' \
		'clean         remove $(BIN)/'

tidy:
	GOWORK=off $(GO) mod tidy

# Upgrade the dependencies to their latest minor/patch releases (PATCH=1: patch releases only), then tidy and
# check that everything still builds and passes. GOWORK=off: resolves against the module proxy, not the sibling
# folders of go.work. Review `git diff go.mod go.sum` before committing.
PATCH ?=
upgrade:
	GOWORK=off $(GO) get -u$(if $(PATCH),=patch) ./...
	GOWORK=off $(GO) mod tidy
	GOWORK=off $(GO) build ./...
	GOWORK=off $(GO) test ./...

# Lists the modules that have a newer version (nothing is changed).
outdated:
	@GOWORK=off $(GO) list -u -m -f '{{if and .Update (not .Indirect)}}{{.Path}} {{.Version}} -> {{.Update.Version}}{{end}}' all

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

build: courier worker courierctl

courier:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/courier ./cmd/courier

worker:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/worker ./cmd/worker

courierctl:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/courierctl ./cmd/courierctl

run-courier:
	$(GO) run ./cmd/courier

run-worker:
	$(GO) run ./cmd/worker

# make send ARGS='--locale vi --template hello --channel telegram --address <chat_id> --data name=Hu'
ARGS ?= --locale en --template hello --channel telegram --address ''
send:
	$(GO) run ./cmd/courierctl $(ARGS)

redis-up:
	$(COMPOSE) up -d

redis-down:
	$(COMPOSE) down

redis-logs:
	$(COMPOSE) logs -f redis

PROTO_REF ?= v0.1.6

generate:
	PROTO_REF=$(PROTO_REF) PROTO_DIR=../protos PROTOGEN_DIR=../protogen bash ../protos/scripts/generate.sh go
	cd ../protogen/go && $(GO) mod tidy

clean:
	rm -rf $(BIN)

docker: docker-courier docker-worker

docker-courier:
	docker build -f build/courier/Dockerfile -t $(IMAGE_REPO)/courier:$(IMAGE_TAG) .

docker-worker:
	docker build -f build/courier-worker/Dockerfile -t $(IMAGE_REPO)/courier-worker:$(IMAGE_TAG) .
