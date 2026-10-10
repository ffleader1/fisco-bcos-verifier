// Package mock is an in-memory Ledger that reproduces the WithdrawalLedger / AuditTrail contract rules exactly
// (same validations, revert codes and events). It lets the service run and be tested without a FISCO BCOS node;
// the conformance suite (package ledgertest) runs the same scenarios against both this mock and a real chain.
//
// The mock keeps a simple block list (one transaction per block, sealed by a single "mock" node) so the
// explorer works in development. State is lost on restart.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"

	"bouyup/fisco/internal/ledger"
)

const (
	maxBanks           = 20
	withdrawalContract = "0x000000000000000000000000000000000000d001"
	auditContract      = "0x000000000000000000000000000000000000a001"
	mockNodeID         = "mock-node"
)

type record struct {
	kind        ledger.AnchorKind
	contentHash ledger.Hash
	ts          int64
	writer      string
	head        ledger.Hash
}

type block struct {
	b  ledger.Block
	tx ledger.TxDetail
}

// Mock implements ledger.Ledger and ledger.Admin.
type Mock struct {
	mu       sync.Mutex
	clock    func() int64
	accounts ledger.Accounts
	roles    map[string]uint64
	txSeq    uint64

	blocks []block        // blocks[i].b.Number == i+1
	txs    map[string]int // tx hash -> index in blocks
	wds    map[string]ledger.Withdrawal
	wdIDs  []string
	wrote  map[int64]time.Time // block number -> when it was written (for the step-by-step demo)

	records     map[ledger.Hash][]record
	heads       map[ledger.Hash]ledger.Hash
	seen        map[ledger.Hash]bool
	firstAnchor map[ledger.Hash][2]any // contentHash -> {subject Hash, index int64}
}

// Option configures the mock.
type Option func(*Mock)

// WithClock injects a clock (unix seconds). Default: time.Now.
func WithClock(f func() int64) Option { return func(m *Mock) { m.clock = f } }

// New creates a mock "deployed" by accounts.Admin.
func New(acc ledger.Accounts, opts ...Option) *Mock {
	m := &Mock{
		clock:       func() int64 { return time.Now().Unix() },
		accounts:    ledger.Accounts{Admin: strings.ToLower(acc.Admin), Operator: strings.ToLower(acc.Operator)},
		roles:       map[string]uint64{},
		txs:         map[string]int{},
		wds:         map[string]ledger.Withdrawal{},
		records:     map[ledger.Hash][]record{},
		heads:       map[ledger.Hash]ledger.Hash{},
		seen:        map[ledger.Hash]bool{},
		firstAnchor: map[ledger.Hash][2]any{},
	}
	for _, o := range opts {
		o(m)
	}
	m.roles[m.accounts.Admin] = ledger.RoleAdmin
	return m
}

func (m *Mock) Close() {}

// ---------------------------------------------------------------- helpers

func rev(kind ledger.ErrorKind, msg string) error { return &ledger.Error{Kind: kind, Message: msg} }

func (m *Mock) require(sender string, role uint64) error {
	if m.roles[sender]&role == 0 {
		return rev(ledger.KindUnauthorized, "missing role")
	}
	return nil
}

func hexHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return "0x" + hex.EncodeToString(h.Sum(nil))
}

// newTx seals a block holding one transaction and returns it.
func (m *Mock) newTx(from, to, contract, method string, events []ledger.Event) ledger.Tx {
	m.txSeq++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], m.txSeq)
	txHash := hexHash([]byte("bouyup-mock-tx"), b[:])
	number := int64(len(m.blocks) + 1)
	parent := "0x" + strings.Repeat("0", 64)
	if len(m.blocks) > 0 {
		parent = m.blocks[len(m.blocks)-1].b.Hash
	}
	blockHash := hexHash([]byte("bouyup-mock-block"), b[:], []byte(parent))
	if events == nil {
		events = []ledger.Event{}
	}
	ts := m.clock()
	blk := ledger.Block{Number: number, Hash: blockHash, ParentHash: parent, Timestamp: ts, Sealer: "node0", SealerNodeID: mockNodeID,
		TxCount: 1, Transactions: []string{txHash}}
	detail := ledger.TxDetail{Hash: txHash, Status: "success", BlockNumber: number, BlockHash: blockHash, Timestamp: ts, From: from,
		FromRole: m.roleName(from), To: to, Contract: contract, Method: method, Events: events, Sealer: "node0", SealerNodeID: mockNodeID,
		SubmittedTo:   "node0 (in-memory mock)",
		Confirmations: []ledger.Confirmation{{Node: "node0", URL: "in-memory", NodeID: mockNodeID, Confirmed: true}}}
	m.blocks = append(m.blocks, block{b: blk, tx: detail})
	if m.wrote == nil {
		m.wrote = map[int64]time.Time{}
	}
	m.wrote[number] = time.Now()
	m.txs[txHash] = len(m.blocks) - 1
	return ledger.Tx{Hash: txHash, BlockNumber: number, BlockHash: blockHash, Events: events}
}

func (m *Mock) roleName(addr string) string {
	switch addr {
	case m.accounts.Operator:
		return "operator"
	case m.accounts.Admin:
		return "admin"
	}
	return ""
}

func ev(name string, kv ...any) ledger.Event {
	f := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i].(string)] = kv[i+1]
	}
	return ledger.Event{Name: name, Fields: f}
}

// ---------------------------------------------------------------- admin

func (m *Mock) GrantRole(_ context.Context, account string, role uint64) (ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.require(m.accounts.Admin, ledger.RoleAdmin); err != nil {
		return ledger.Tx{}, err
	}
	a := strings.ToLower(account)
	m.roles[a] |= role
	return m.newTx(m.accounts.Admin, withdrawalContract, "WithdrawalLedger", "grantRole", []ledger.Event{ev("RoleGranted", "account", a, "role", int64(role))}), nil
}

func (m *Mock) HasRole(_ context.Context, account string, role uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.roles[strings.ToLower(account)]&role != 0, nil
}

// ---------------------------------------------------------------- info

func (m *Mock) Info(context.Context) (ledger.ChainInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ledger.ChainInfo{Backend: "mock", BlockNumber: int64(len(m.blocks)), ChainTime: m.clock(),
		Contracts: map[string]string{"WithdrawalLedger": withdrawalContract, "AuditTrail": auditContract},
		Accounts:  map[string]string{"admin": m.accounts.Admin, "operator": m.accounts.Operator}}, nil
}

func (m *Mock) Now(context.Context) (int64, error) { return m.clock(), nil }

// ---------------------------------------------------------------- withdrawals

func (m *Mock) RecordWithdrawal(_ context.Context, r ledger.WithdrawalRequest) (ledger.Withdrawal, ledger.Tx, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writer := m.accounts.Operator
	if err := m.require(writer, ledger.RoleOperator); err != nil {
		return ledger.Withdrawal{}, ledger.Tx{}, err
	}
	if err := ValidateWithdrawal(r); err != nil {
		return ledger.Withdrawal{}, ledger.Tx{}, err
	}
	if _, ok := m.wds[r.ID]; ok {
		return ledger.Withdrawal{}, ledger.Tx{}, rev(ledger.KindDuplicate, "withdrawal already recorded")
	}
	ts := m.clock()
	tx := m.newTx(writer, withdrawalContract, "WithdrawalLedger", "record", []ledger.Event{ev("WithdrawalRecorded", "withdrawalId", r.ID,
		"root", r.Root.Hex(), "pieces", int64(len(r.Commitments)), "envelopeBytes", int64(len(r.Envelope)), "timestamp", ts)})
	w := ledger.Withdrawal{ID: r.ID, Root: r.Root, Commitments: append([]ledger.Hash(nil), r.Commitments...), Envelope: append([]byte(nil), r.Envelope...),
		Timestamp: ts, BlockNumber: tx.BlockNumber, Writer: writer, TxHash: tx.Hash}
	m.wds[r.ID] = w
	m.wdIDs = append(m.wdIDs, r.ID)
	return w, tx, nil
}

// ValidateWithdrawal applies exactly the checks of WithdrawalLedger.record.
func ValidateWithdrawal(r ledger.WithdrawalRequest) error {
	if _, err := ledger.IDToBytes32(r.ID); err != nil {
		return err
	}
	if n := len(r.Commitments); n < 2 || n > 21 {
		return rev(ledger.KindInvalid, "2-21 pieces")
	}
	if len(r.Envelope) == 0 || len(r.Envelope) > 32768 {
		return rev(ledger.KindInvalid, "envelope size")
	}
	for _, c := range r.Commitments {
		if c.IsZero() {
			return rev(ledger.KindInvalid, "empty commitment")
		}
	}
	if r.Root != ledger.MerkleRoot(r.Commitments) {
		return rev(ledger.KindInvalid, "root does not match the commitments")
	}
	return nil
}

func (m *Mock) GetWithdrawal(_ context.Context, id string) (ledger.Withdrawal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := ledger.IDToBytes32(id); err != nil {
		return ledger.Withdrawal{}, err
	}
	w, ok := m.wds[id]
	if !ok {
		return ledger.Withdrawal{}, rev(ledger.KindNotFound, "withdrawal")
	}
	w.Commitments = append([]ledger.Hash(nil), w.Commitments...)
	w.Envelope = append([]byte(nil), w.Envelope...)
	return w, nil
}

func (m *Mock) ListWithdrawals(_ context.Context, offset, limit int) ([]ledger.Withdrawal, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := len(m.wdIDs)
	out := []ledger.Withdrawal{}
	if offset < 0 {
		offset = 0
	}
	for i := total - 1 - offset; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, m.wds[m.wdIDs[i]])
	}
	return out, total, nil
}

// ---------------------------------------------------------------- audit trail

func (m *Mock) Anchor(_ context.Context, r ledger.AnchorRequest) (ledger.AnchorRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writer := m.accounts.Operator
	if err := m.require(writer, ledger.RoleAuditWriter); err != nil {
		return ledger.AnchorRecord{}, err
	}
	subj, err := ledger.IDToBytes32(r.Subject)
	if err != nil {
		return ledger.AnchorRecord{}, err
	}
	switch {
	case r.Kind < ledger.KindApplication || r.Kind > ledger.KindOther:
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "kind")
	case r.ContentHash.IsZero():
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "contentHash")
	case len(r.URI) > 256:
		return ledger.AnchorRecord{}, rev(ledger.KindInvalid, "uri too long")
	}
	seenKey := ledger.Hash(crypto.Keccak256Hash(subj[:], r.ContentHash[:]))
	if m.seen[seenKey] {
		return ledger.AnchorRecord{}, rev(ledger.KindDuplicate, "already anchored for this subject")
	}
	ts := m.clock()
	head := ledger.ChainHead(m.heads[subj], r.Kind, r.ContentHash, ts)
	idx := int64(len(m.records[subj]))
	m.records[subj] = append(m.records[subj], record{kind: r.Kind, contentHash: r.ContentHash, ts: ts, writer: writer, head: head})
	m.heads[subj] = head
	m.seen[seenKey] = true
	if _, ok := m.firstAnchor[r.ContentHash]; !ok {
		m.firstAnchor[r.ContentHash] = [2]any{subj, idx}
	}
	tx := m.newTx(writer, auditContract, "AuditTrail", "anchor", []ledger.Event{ev("Anchored", "subject", r.Subject, "index", idx, "kind", int64(r.Kind),
		"contentHash", r.ContentHash.Hex(), "head", head.Hex(), "timestamp", ts, "uri", r.URI)})
	return ledger.AnchorRecord{Subject: r.Subject, Index: idx, Kind: r.Kind, ContentHash: r.ContentHash, Timestamp: ts, Writer: writer, Head: head, Tx: &tx}, nil
}

func (m *Mock) ListAnchors(_ context.Context, subject string) ([]ledger.AnchorRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subj, err := ledger.IDToBytes32(subject)
	if err != nil {
		return nil, err
	}
	out := []ledger.AnchorRecord{}
	for i, r := range m.records[subj] {
		out = append(out, ledger.AnchorRecord{Subject: subject, Index: int64(i), Kind: r.kind, ContentHash: r.contentHash, Timestamp: r.ts, Writer: r.writer, Head: r.head})
	}
	return out, nil
}

func (m *Mock) LocateAnchor(_ context.Context, contentHash ledger.Hash) (ledger.AnchorRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.firstAnchor[contentHash]
	if !ok {
		return ledger.AnchorRecord{}, false, nil
	}
	subj, idx := f[0].(ledger.Hash), f[1].(int64)
	r := m.records[subj][idx]
	return ledger.AnchorRecord{Subject: ledger.Bytes32ToID(subj), Index: idx, Kind: r.kind, ContentHash: r.contentHash, Timestamp: r.ts, Writer: r.writer, Head: r.head}, true, nil
}

// ---------------------------------------------------------------- explorer

func (m *Mock) Nodes(context.Context) ([]ledger.NodeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return []ledger.NodeInfo{{Name: "node0", URL: "in-memory", NodeID: mockNodeID, BlockNumber: int64(len(m.blocks)), Online: true, Sealer: true}}, nil
}

// BlockCommits implements ledger.CommitWatcher: the mock is a single node that writes a block the moment it is made.
func (m *Mock) BlockCommits(_ context.Context, n int64, _ time.Duration) []ledger.NodeCommit {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.wrote[n]
	return []ledger.NodeCommit{{Node: "node0", At: at, Seen: ok}}
}

func (m *Mock) LatestBlocks(_ context.Context, limit int) ([]ledger.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ledger.Block{}
	for i := len(m.blocks) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.blocks[i].b)
	}
	return out, nil
}

func (m *Mock) GetBlock(_ context.Context, number int64) (ledger.Block, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if number < 1 || number > int64(len(m.blocks)) {
		return ledger.Block{}, rev(ledger.KindNotFound, "block")
	}
	return m.blocks[number-1].b, nil
}

func (m *Mock) GetTransaction(_ context.Context, hash string) (ledger.TxDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.txs[strings.ToLower(hash)]
	if !ok {
		return ledger.TxDetail{}, rev(ledger.KindNotFound, "transaction")
	}
	return m.blocks[i].tx, nil
}

var _ ledger.Ledger = (*Mock)(nil)
var _ ledger.Admin = (*Mock)(nil)
