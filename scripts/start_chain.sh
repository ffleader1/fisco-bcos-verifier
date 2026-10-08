#!/usr/bin/env bash
# Start a local 4-node FISCO BCOS v2.11.1 chain (group 1, chain id 1) for SellerGraph development.
# Runs on Linux, macOS (Intel, via Docker mode) or Windows through WSL2 (Ubuntu). Requires curl, openssl.
#
#   ./scripts/start_chain.sh            # native binary (Linux / WSL2)
#   ./scripts/start_chain.sh --docker   # nodes run in the fiscoorg/fiscobcos:v2.11.1 image (Linux, host network)
#
# Result: channel port 127.0.0.1:20200 (what sgchain connects to), SDK certs copied to ./chain/sdk/.
# CHAIN_DIR=~/sg-chain ./scripts/start_chain.sh  keeps node data outside the repo (recommended on WSL2 when the
# repo lives on the Windows drive: node databases are much faster on the Linux filesystem).
set -euo pipefail
VERSION=2.11.1
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="${CHAIN_DIR:-$ROOT/chain}"
mkdir -p "$DIR" "$ROOT/chain/sdk"
cd "$DIR"

if [ -d nodes/127.0.0.1 ]; then
  echo "chain already generated in $DIR/nodes — starting it"
else
  [ -f build_chain.sh ] || curl -fsSL -o build_chain.sh "https://raw.githubusercontent.com/FISCO-BCOS/FISCO-BCOS/v${VERSION}/tools/build_chain.sh"
  if [ "${1:-}" = "--docker" ]; then
    bash build_chain.sh -l 127.0.0.1:4 -p 30300,20200,8545 -d -v "$VERSION"
  else
    if [ ! -x fisco-bcos ]; then
      case "$(uname -s)" in
        Linux) curl -fsSL -o fisco-bcos.tar.gz "https://github.com/FISCO-BCOS/FISCO-BCOS/releases/download/v${VERSION}/fisco-bcos.tar.gz" ;;
        Darwin) curl -fsSL -o fisco-bcos.tar.gz "https://github.com/FISCO-BCOS/FISCO-BCOS/releases/download/v${VERSION}/fisco-bcos-macOS.tar.gz" ;;
        *) echo "unsupported OS: use WSL2 on Windows"; exit 1 ;;
      esac
      tar xzf fisco-bcos.tar.gz && rm -f fisco-bcos.tar.gz
    fi
    ./fisco-bcos -v
    bash build_chain.sh -l 127.0.0.1:4 -p 30300,20200,8545 -e ./fisco-bcos
  fi
fi

cp nodes/127.0.0.1/sdk/ca.crt nodes/127.0.0.1/sdk/sdk.crt nodes/127.0.0.1/sdk/sdk.key "$ROOT/chain/sdk/"
bash nodes/127.0.0.1/start_all.sh
sleep 3
curl -s -X POST --data '{"jsonrpc":"2.0","method":"getBlockNumber","params":[1],"id":1}' http://127.0.0.1:8545 && echo
echo "FISCO BCOS v${VERSION} is up: channel 127.0.0.1:20200, nodes in $DIR, SDK certs in $ROOT/chain/sdk"
