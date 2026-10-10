package fisco

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/FISCO-BCOS/go-sdk/core/types"
	"github.com/ethereum/go-ethereum/common"

	"buoyup/fisco/internal/ledger"
)

// nodeIDsMu serializes the GetSyncStatus fan-out of nodeNames.
var nodeIDsMu sync.Mutex

// nodeNames maps node id -> node name (node0, node1, ...) for the connected nodes.
func (l *Ledger) nodeNames(ctx context.Context) map[string]string {
	nodeIDsMu.Lock()
	defer nodeIDsMu.Unlock()
	out := map[string]string{}
	for _, n := range l.nodes {
		if n.c == nil {
			continue
		}
		if st, err := n.c.GetSyncStatus(ctx); err == nil && st != nil && st.NodeId != "" {
			out[strings.ToLower(st.NodeId)] = n.name
		}
	}
	return out
}

func (l *Ledger) sealerList(ctx context.Context) []string {
	raw, err := l.reader().GetSealerList(ctx)
	if err != nil {
		return nil
	}
	var ids []string
	_ = json.Unmarshal(raw, &ids)
	return ids
}

// Nodes reports every configured node: reachable or not, its node id and block height.
func (l *Ledger) Nodes(ctx context.Context) ([]ledger.NodeInfo, error) {
	sealers := map[string]bool{}
	for _, id := range l.sealerList(ctx) {
		sealers[strings.ToLower(id)] = true
	}
	out := []ledger.NodeInfo{}
	for _, n := range l.nodes {
		info := ledger.NodeInfo{Name: n.name, URL: n.url}
		if n.c == nil {
			info.Error = "not connected"
			out = append(out, info)
			continue
		}
		bn, err := n.c.GetBlockNumber(ctx)
		if err != nil {
			info.Error = err.Error()
			out = append(out, info)
			continue
		}
		info.Online, info.BlockNumber = true, bn
		if st, err := n.c.GetSyncStatus(ctx); err == nil && st != nil {
			info.NodeID = st.NodeId
			info.Sealer = sealers[strings.ToLower(st.NodeId)]
		}
		out = append(out, info)
	}
	return out, nil
}

func txHashes(list []interface{}) []string {
	out := []string{}
	for _, t := range list {
		switch v := t.(type) {
		case string:
			out = append(out, v)
		case map[string]interface{}:
			if h, ok := v["hash"].(string); ok {
				out = append(out, h)
			}
		}
	}
	return out
}

func (l *Ledger) toBlock(b *types.Block, names map[string]string) ledger.Block {
	out := ledger.Block{Number: hexInt(b.Number), Hash: b.Hash, ParentHash: b.ParentHash, Timestamp: hexInt(b.Timestamp) / 1000,
		Transactions: txHashes(b.Transactions)}
	out.TxCount = len(out.Transactions)
	if i := int(hexInt(b.Sealer)); i >= 0 && i < len(b.SealerList) {
		out.SealerNodeID = b.SealerList[i]
		out.Sealer = names[strings.ToLower(b.SealerList[i])]
		if out.Sealer == "" {
			out.Sealer = fmt.Sprintf("sealer #%d", i)
		}
	}
	return out
}

func (l *Ledger) LatestBlocks(ctx context.Context, limit int) ([]ledger.Block, error) {
	bn, err := l.reader().GetBlockNumber(ctx)
	if err != nil {
		return nil, ledger.Errorf(ledger.KindChain, "getBlockNumber: %v", err)
	}
	names := l.nodeNames(ctx)
	out := []ledger.Block{}
	for n := bn; n >= 1 && len(out) < limit; n-- {
		b, err := l.reader().GetBlockByNumber(ctx, n, false)
		if err != nil {
			return nil, ledger.Errorf(ledger.KindChain, "getBlockByNumber(%d): %v", n, err)
		}
		out = append(out, l.toBlock(b, names))
	}
	return out, nil
}

func (l *Ledger) GetBlock(ctx context.Context, number int64) (ledger.Block, error) {
	bn, err := l.reader().GetBlockNumber(ctx)
	if err != nil {
		return ledger.Block{}, ledger.Errorf(ledger.KindChain, "getBlockNumber: %v", err)
	}
	if number < 0 || number > bn {
		return ledger.Block{}, ledger.Errorf(ledger.KindNotFound, "block")
	}
	b, err := l.reader().GetBlockByNumber(ctx, number, false)
	if err != nil {
		return ledger.Block{}, ledger.Errorf(ledger.KindChain, "getBlockByNumber(%d): %v", number, err)
	}
	return l.toBlock(b, l.nodeNames(ctx)), nil
}

func (l *Ledger) GetTransaction(ctx context.Context, hash string) (ledger.TxDetail, error) {
	h := common.HexToHash(hash)
	rc, err := l.reader().GetTransactionReceipt(ctx, h)
	if err != nil || rc == nil || rc.TransactionHash == "" {
		return ledger.TxDetail{}, ledger.Errorf(ledger.KindNotFound, "transaction")
	}
	d := ledger.TxDetail{Hash: strings.ToLower(rc.TransactionHash), BlockNumber: hexInt(rc.BlockNumber), BlockHash: rc.BlockHash,
		Index: hexInt(rc.TransactionIndex), From: strings.ToLower(rc.From), To: strings.ToLower(rc.To), Status: "success",
		Events: []ledger.Event{}, SubmittedTo: "node0 (" + l.node.NodeURL + ")"}
	if rc.Status != types.Success {
		d.Status = "failed"
	}
	switch d.From {
	case l.accounts.Operator:
		d.FromRole = "operator"
	case l.accounts.Admin:
		d.FromRole = "admin"
	}
	var parsedABI = l.wdABI
	switch common.HexToAddress(d.To) {
	case l.wdAt:
		d.Contract = cWithdrawal
	case l.auditAt:
		d.Contract, parsedABI = cAudit, l.auditABI
	}
	if in := common.FromHex(rc.Input); len(in) >= 4 && d.Contract != "" {
		if m, err := parsedABI.MethodByID(in[:4]); err == nil {
			d.Method = m.Name
		}
	}
	for _, lg := range rc.Logs {
		if e, ok := decodeLog(lg, l.wdABI, l.auditABI); ok {
			d.Events = append(d.Events, e)
		}
	}
	names := l.nodeNames(ctx)
	if b, err := l.reader().GetBlockByNumber(ctx, d.BlockNumber, false); err == nil && b != nil {
		blk := l.toBlock(b, names)
		d.Timestamp, d.Sealer, d.SealerNodeID = blk.Timestamp, blk.Sealer, blk.SealerNodeID
	}
	idOf := map[string]string{}
	for id, name := range names {
		idOf[name] = id
	}
	for _, n := range l.nodes {
		c := ledger.Confirmation{Node: n.name, URL: n.url, NodeID: idOf[n.name]}
		if n.c == nil {
			c.Error = "not connected"
		} else if r, err := n.c.GetTransactionReceipt(ctx, h); err != nil {
			if !strings.Contains(err.Error(), "not on-chain") { // "not on-chain" = this node has not caught up yet
				c.Error = err.Error()
			}
		} else {
			c.Confirmed = r != nil && strings.EqualFold(r.TransactionHash, d.Hash)
		}
		d.Confirmations = append(d.Confirmations, c)
	}
	return d, nil
}
