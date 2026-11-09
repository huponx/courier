GO      ?= go
BIN     ?= bin
COMPOSE ?= docker compose
IMAGE_TAG ?= dev
IMAGE_REPO ?= ghcr.io/huponx

.PHONY: help tidy test vet build docker docker-courier docker-worker \
	courier worker courierctl \
	run-courier run-worker send \
	redis-up redis-down redis-logs \
	generate clean

help:
	@printf '%s\n' \
		'tidy          go mod tidy' \
		'test          go test ./...' \
		'vet           go vet ./...' \
		'build         build courier, worker, courierctl into $(BIN)/' \
		'run-courier   go run ./cmd/courier' \
		'run-worker    go run ./cmd/worker' \
		'send          courierctl (override with ARGS=...)' \
		'redis-up      docker compose up -d' \
		'redis-down    docker compose down' \
		'docker        build both images (IMAGE_TAG=dev)' \
		'generate      buf generate Go stubs (PROTO_REF=vX.Y.Z; needs replace in go.mod)' \
		'clean         remove $(BIN)/'

tidy:
	$(GO) mod tidy

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
