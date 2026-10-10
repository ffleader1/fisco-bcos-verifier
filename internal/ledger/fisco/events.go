package fisco

import (
	"math/big"
	"reflect"
	"strings"

	"github.com/FISCO-BCOS/go-sdk/abi"
	"github.com/FISCO-BCOS/go-sdk/core/types"
	"github.com/ethereum/go-ethereum/common"

	"buoyup/fisco/internal/ledger"
)

// decodeLog decodes a receipt log emitted by one of the contracts into a ledger.Event with JSON-friendly values,
// using the same conventions as the mock: ids and codes as ASCII, other bytes32 as 0x-hex, addresses
// lowercase hex, integers as int64.
func decodeLog(lg *types.NewLog, abis ...abi.ABI) (ledger.Event, bool) {
	if lg == nil || len(lg.Topics) == 0 {
		return ledger.Event{}, false
	}
	topic0 := common.HexToHash(lg.Topics[0])
	var ev *abi.Event
	for _, a := range abis {
		for _, e := range a.Events {
			if e.ID() == topic0 {
				ev = e
			}
		}
	}
	if ev == nil {
		return ledger.Event{}, false
	}
	fields := map[string]any{}
	vals, err := ev.Inputs.NonIndexed().UnpackValues(common.FromHex(lg.Data))
	if err != nil {
		return ledger.Event{}, false
	}
	ti, vi := 1, 0
	for _, in := range ev.Inputs {
		var v any
		if in.Indexed {
			if ti >= len(lg.Topics) {
				return ledger.Event{}, false
			}
			t := common.HexToHash(lg.Topics[ti])
			ti++
			switch in.Type.T {
			case abi.AddressTy:
				v = common.BytesToAddress(t[12:])
			case abi.UintTy, abi.IntTy:
				v = new(big.Int).SetBytes(t[:])
			default:
				v = [32]byte(t)
			}
		} else {
			v = vals[vi]
			vi++
		}
		fields[in.Name] = normalize(in.Name, v)
	}
	return ledger.Event{Name: ev.Name, Fields: fields}, true
}

func normalize(name string, v any) any {
	switch x := v.(type) {
	case *big.Int:
		return x.Int64()
	case uint8:
		return int64(x)
	case uint64:
		return int64(x)
	case bool, string:
		return x
	case common.Address:
		return strings.ToLower(x.Hex())
	case [32]byte:
		switch name {
		case "withdrawalId", "subject", "code", "currency", "region", "province", "actorId", "bank":
			return ledger.Bytes32ToID(ledger.Hash(x))
		}
		return ledger.Hash(x).Hex()
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Array || rv.Kind() == reflect.Slice {
		out := make([]int64, rv.Len())
		for i := range out {
			if b, ok := rv.Index(i).Interface().(*big.Int); ok {
				out[i] = b.Int64()
			}
		}
		return out
	}
	return v
}
