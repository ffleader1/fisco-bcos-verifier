package fisco

import (
	"context"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"buoyup/fisco/internal/ledger"
)

// ---------------------------------------------------------------- withdrawals

func hashesArg(hs []ledger.Hash) [][32]byte {
	out := make([][32]byte, len(hs))
	for i, h := range hs {
		out[i] = b32(h)
	}
	return out
}

func (l *Ledger) RecordWithdrawal(ctx context.Context, r ledger.WithdrawalRequest) (ledger.Withdrawal, ledger.Tx, error) {
	id, err := idArg(r.ID)
	if err != nil {
		return ledger.Withdrawal{}, ledger.Tx{}, err
	}
	tx, err := l.transact(ctx, l.operator, cWithdrawal, "record", id, b32(r.Root), hashesArg(r.Commitments), r.Envelope)
	if err != nil {
		return ledger.Withdrawal{}, ledger.Tx{}, err
	}
	l.cacheMu.Lock()
	l.txOf[r.ID] = tx.Hash
	l.cacheMu.Unlock()
	w, err := l.GetWithdrawal(ctx, r.ID)
	return w, tx, err
}

func (l *Ledger) GetWithdrawal(ctx context.Context, wid string) (ledger.Withdrawal, error) {
	id, err := idArg(wid)
	if err != nil {
		return ledger.Withdrawal{}, err
	}
	v, err := l.call(ctx, cWithdrawal, "getWithdrawal", id)
	if err != nil {
		return ledger.Withdrawal{}, err
	}
	w := ledger.Withdrawal{ID: wid, Root: h32(v[0]), Envelope: v[2].([]byte), Timestamp: i64(v[3]), BlockNumber: i64(v[4]),
		Writer: strings.ToLower(v[5].(common.Address).Hex()), Commitments: []ledger.Hash{}}
	for _, c := range v[1].([][32]byte) {
		w.Commitments = append(w.Commitments, ledger.Hash(c))
	}
	w.TxHash = l.findWithdrawalTx(ctx, wid, w.BlockNumber)
	return w, nil
}

// findWithdrawalTx returns the hash of the transaction that recorded the withdrawal, by scanning its block.
func (l *Ledger) findWithdrawalTx(ctx context.Context, wid string, blockNumber int64) string {
	l.cacheMu.Lock()
	h, ok := l.txOf[wid]
	l.cacheMu.Unlock()
	if ok {
		return h
	}
	b, err := l.reader().GetBlockByNumber(ctx, blockNumber, false)
	if err != nil || b == nil {
		return ""
	}
	topic := l.wdABI.Events["WithdrawalRecorded"].ID()
	want, _ := idArg(wid)
	for _, t := range txHashes(b.Transactions) {
		rc, err := l.reader().GetTransactionReceipt(ctx, common.HexToHash(t))
		if err != nil || rc == nil {
			continue
		}
		for _, lg := range rc.Logs {
			if len(lg.Topics) > 1 && common.HexToHash(lg.Topics[0]) == topic && common.HexToHash(lg.Topics[1]) == common.Hash(want) {
				l.cacheMu.Lock()
				l.txOf[wid] = t
				l.cacheMu.Unlock()
				return t
			}
		}
	}
	return ""
}

func (l *Ledger) ListWithdrawals(ctx context.Context, offset, limit int) ([]ledger.Withdrawal, int, error) {
	n, err := l.call(ctx, cWithdrawal, "withdrawalCount")
	if err != nil {
		return nil, 0, err
	}
	total := int(i64(n[0]))
	out := []ledger.Withdrawal{}
	if offset < 0 {
		offset = 0
	}
	for i := total - 1 - offset; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		v, err := l.call(ctx, cWithdrawal, "withdrawalIds", bi(int64(i)))
		if err != nil {
			return nil, 0, err
		}
		w, err := l.GetWithdrawal(ctx, ledger.Bytes32ToID(h32(v[0])))
		if err != nil {
			return nil, 0, err
		}
		out = append(out, w)
	}
	return out, total, nil
}

// ---------------------------------------------------------------- audit

func (l *Ledger) Anchor(ctx context.Context, r ledger.AnchorRequest) (ledger.AnchorRecord, error) {
	subj, err := idArg(r.Subject)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	tx, err := l.transact(ctx, l.operator, cAudit, "anchor", subj, uint8(r.Kind), b32(r.ContentHash), r.URI)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	for _, e := range tx.Events {
		if e.Name == "Anchored" {
			head, _ := ledger.ParseHash(e.Fields["head"].(string))
			return ledger.AnchorRecord{Subject: r.Subject, Index: e.Fields["index"].(int64), Kind: r.Kind, ContentHash: r.ContentHash,
				Timestamp: e.Fields["timestamp"].(int64), Writer: l.accounts.Operator, Head: head, Tx: &tx}, nil
		}
	}
	return ledger.AnchorRecord{}, ledger.Errorf(ledger.KindChain, "Anchored event missing")
}

func (l *Ledger) record(ctx context.Context, subj [32]byte, idx int64) (ledger.AnchorRecord, error) {
	v, err := l.call(ctx, cAudit, "getRecord", subj, bi(idx))
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	return ledger.AnchorRecord{Subject: ledger.Bytes32ToID(ledger.Hash(subj)), Index: idx, Kind: ledger.AnchorKind(v[0].(uint8)),
		ContentHash: h32(v[1]), Timestamp: i64(v[2]), Writer: strings.ToLower(v[3].(common.Address).Hex()), Head: h32(v[4])}, nil
}

func (l *Ledger) ListAnchors(ctx context.Context, subject string) ([]ledger.AnchorRecord, error) {
	subj, err := idArg(subject)
	if err != nil {
		return nil, err
	}
	n, err := l.call(ctx, cAudit, "recordCount", subj)
	if err != nil {
		return nil, err
	}
	out := []ledger.AnchorRecord{}
	for i := int64(0); i < i64(n[0]); i++ {
		r, err := l.record(ctx, subj, i)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (l *Ledger) LocateAnchor(ctx context.Context, contentHash ledger.Hash) (ledger.AnchorRecord, bool, error) {
	v, err := l.call(ctx, cAudit, "locate", b32(contentHash))
	if err != nil {
		return ledger.AnchorRecord{}, false, err
	}
	if !v[0].(bool) {
		return ledger.AnchorRecord{}, false, nil
	}
	r, err := l.record(ctx, v[1].([32]byte), i64(v[2]))
	return r, err == nil, err
}
