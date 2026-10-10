# BuoyUp FISCO BCOS component
BIN ?= bin/sgchain
FISCO_SDK_DIR ?= $(CURDIR)/chain/sdk

.PHONY: tidy build test test-integration contracts chain chain-stop keys deploy run run-mock demo

tidy:             ## resolve go.sum (run once after cloning)
	go mod tidy

build:
	go build -o $(BIN) ./cmd/sgchain

test:             ## unit + API + mock conformance tests (no chain needed)
	go test ./...

test-integration: ## conformance suite against a live chain (run `make chain` first)
	FISCO_SDK_DIR=$(FISCO_SDK_DIR) FISCO_PEERS=127.0.0.1:20201,127.0.0.1:20202,127.0.0.1:20203 go test -tags integration -count=1 -v ./internal/ledger/fisco/

contracts:        ## recompile Solidity (needs Node.js; solc 0.6.10, same as the FISCO BCOS 2.x console)
	cd scripts && (test -d node_modules/solc || npm i --no-save solc@0.6.10) && node compile_contracts.js ../contracts ../contracts/build

chain:            ## local 4-node FISCO BCOS v2.11.1 (Linux / WSL2)
	./scripts/start_chain.sh

chain-stop:
	./scripts/stop_chain.sh

keys: build
	$(BIN) keygen -out keys -names admin,operator

deploy: build
	$(BIN) deploy -config configs/fisco.json

run: build
	$(BIN) serve -config configs/fisco.json

run-mock: build
	$(BIN) serve -config configs/mock.json

demo:
	./scripts/demo.sh
