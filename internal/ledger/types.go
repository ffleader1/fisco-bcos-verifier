// Package ledger defines the BuoyUp blockchain ledger: domain types and the Ledger interface implemented by
// the FISCO BCOS adapter (package fisco) and the in-memory mock (package mock).
//
// Withdrawals reach the ledger already encrypted (package sealing): the chain stores an envelope, the
// commitments of its pieces and their Merkle root. Times are unix seconds. Hash is a 32-byte value.
package ledger

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Hash is a 32-byte value as stored on chain (bytes32).
type Hash [32]byte

// ZeroHash is the empty bytes32.
var ZeroHash Hash

func (h Hash) IsZero() bool                 { return h == ZeroHash }
func (h Hash) Hex() string                  { return "0x" + hex.EncodeToString(h[:]) }
func (h Hash) String() string               { return h.Hex() }
func (h Hash) MarshalText() ([]byte, error) { return []byte(h.Hex()), nil }
func (h *Hash) UnmarshalText(b []byte) error {
	p, err := ParseHash(string(b))
	if err != nil {
		return err
	}
	*h = p
	return nil
}

// ParseHash parses a 0x-prefixed (or bare) 64-char hex string.
func ParseHash(s string) (Hash, error) {
	var h Hash
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) != 64 {
		return h, fmt.Errorf("hash must be 32 bytes hex, got %d hex chars", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("invalid hex: %w", err)
	}
	copy(h[:], b)
	return h, nil
}

// ---------------------------------------------------------------- enums

// AnchorKind mirrors AuditTrail K_* constants.
type AnchorKind uint8

const (
	KindApplication     AnchorKind = 1
	KindAlgorithmOutput AnchorKind = 2
	KindAgentIO         AnchorKind = 3
	KindCreditMemo      AnchorKind = 4
	KindRiskChallenge   AnchorKind = 5
	KindHumanDecision   AnchorKind = 6
	KindOfferContract   AnchorKind = 7
	KindExplanation     AnchorKind = 8
	KindSweepNotice     AnchorKind = 9
	KindDecisionPackage AnchorKind = 10
	KindOther           AnchorKind = 11
)

var kindNames = []string{"", "application", "algorithm_output", "agent_io", "credit_memo", "risk_challenge", "human_decision",
	"offer_contract", "explanation", "sweep_notice", "decision_package", "other"}

// kindAliases keeps older names working.
var kindAliases = map[string]AnchorKind{"ml_output": KindAlgorithmOutput}

func (k AnchorKind) String() string {
	if int(k) < len(kindNames) && k > 0 {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}
func (k AnchorKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }
func (k *AnchorKind) UnmarshalText(b []byte) error {
	for i, n := range kindNames {
		if i > 0 && strings.EqualFold(n, string(b)) {
			*k = AnchorKind(i)
			return nil
		}
	}
	if a, ok := kindAliases[strings.ToLower(string(b))]; ok {
		*k = a
		return nil
	}
	return fmt.Errorf("unknown anchor kind %q (want one of %s)", b, strings.Join(kindNames[1:], ", "))
}

// ActorType mirrors WithdrawalLedger ACTOR_* constants: who asked for a withdrawal.
type ActorType uint8

const (
	ActorAgent ActorType = 1
	ActorHuman ActorType = 2
)

func (a ActorType) String() string {
	switch a {
	case ActorAgent:
		return "agent"
	case ActorHuman:
		return "human"
	}
	return fmt.Sprintf("actor(%d)", uint8(a))
}
func (a ActorType) MarshalText() ([]byte, error) { return []byte(a.String()), nil }
func (a *ActorType) UnmarshalText(b []byte) error {
	switch strings.ToLower(string(b)) {
	case "agent":
		*a = ActorAgent
	case "human":
		*a = ActorHuman
	default:
		return fmt.Errorf("actor type must be agent or human, got %q", b)
	}
	return nil
}

// Role bits mirror Roles.sol.
const (
	RoleAdmin       uint64 = 1
	RoleOperator    uint64 = 2
	RoleAuditWriter uint64 = 32
)

// TotalBps is 100.00% in basis points.
const TotalBps = 10000

// ---------------------------------------------------------------- withdrawals

// WithdrawalRequest is what the service writes on chain for one payout: nothing readable except the id.
type WithdrawalRequest struct {
	ID          string // WD-...; 1-32 chars of [A-Za-z0-9._:-]
	Root        Hash   // MerkleRoot(Commitments)
	Commitments []Hash // SHA-256 of each piece's plaintext: [0] summary, then one per bank
	Envelope    []byte // encrypted pieces + key slots (package sealing)
}

// Withdrawal is the on-chain record.
type Withdrawal struct {
	ID          string `json:"withdrawalId"`
	Root        Hash   `json:"root"`
	Commitments []Hash `json:"commitments"`
	Envelope    []byte `json:"-"`
	Timestamp   int64  `json:"timestamp"`
	BlockNumber int64  `json:"blockNumber"`
	Writer      string `json:"writer"`
	TxHash      string `json:"txHash,omitempty"`
}

// ---------------------------------------------------------------- audit

type AnchorRequest struct {
	Subject     string
	Kind        AnchorKind
	ContentHash Hash
	URI         string
}

type AnchorRecord struct {
	Subject     string     `json:"subject"`
	Index       int64      `json:"index"`
	Kind        AnchorKind `json:"kind"`
	ContentHash Hash       `json:"contentHash"`
	Timestamp   int64      `json:"timestamp"`
	Writer      string     `json:"writer"`
	Head        Hash       `json:"head"`
	Tx          *Tx        `json:"tx,omitempty"`
}

// ---------------------------------------------------------------- chain

// Event is a decoded contract event. Field names follow the Solidity ABI; values are JSON-friendly
// (int64 for integers, 0x-hex strings for hashes and addresses, ASCII for codes and ids).
type Event struct {
	Name   string         `json:"name"`
	Fields map[string]any `json:"fields"`
}

// Tx is the result of a state-changing call.
type Tx struct {
	Hash        string  `json:"hash"`
	BlockNumber int64   `json:"blockNumber"`
	BlockHash   string  `json:"blockHash,omitempty"`
	Events      []Event `json:"events"`
}

// NodeInfo describes one chain node the service can reach.
type NodeInfo struct {
	Name        string `json:"name"` // node0, node1, ...
	URL         string `json:"url"`
	NodeID      string `json:"nodeId,omitempty"`
	BlockNumber int64  `json:"blockNumber"`
	Online      bool   `json:"online"`
	Sealer      bool   `json:"sealer"`
	Error       string `json:"error,omitempty"`
}

// Block is a block summary for the explorer.
type Block struct {
	Number       int64    `json:"number"`
	Hash         string   `json:"hash"`
	ParentHash   string   `json:"parentHash"`
	Timestamp    int64    `json:"timestamp"`
	Sealer       string   `json:"sealer"`       // node name (node0..) when known
	SealerNodeID string   `json:"sealerNodeId"` // node id
	TxCount      int      `json:"txCount"`
	Transactions []string `json:"transactions"`
}

// Confirmation says whether one node has the transaction receipt.
type Confirmation struct {
	Node      string `json:"node"`
	URL       string `json:"url"`
	NodeID    string `json:"nodeId,omitempty"`
	Confirmed bool   `json:"confirmed"`
	Error     string `json:"error,omitempty"`
}

// TxDetail is one transaction as shown by the explorer.
type TxDetail struct {
	Hash          string         `json:"hash"`
	Status        string         `json:"status"` // success | failed
	BlockNumber   int64          `json:"blockNumber"`
	BlockHash     string         `json:"blockHash"`
	Timestamp     int64          `json:"timestamp"`
	Index         int64          `json:"index"`
	From          string         `json:"from"`
	FromRole      string         `json:"fromRole,omitempty"`
	To            string         `json:"to"`
	Contract      string         `json:"contract,omitempty"` // WithdrawalLedger | AuditTrail
	Method        string         `json:"method,omitempty"`
	Events        []Event        `json:"events"`
	Sealer        string         `json:"sealer"`
	SealerNodeID  string         `json:"sealerNodeId"`
	SubmittedTo   string         `json:"submittedTo"` // node the service sends transactions to
	Confirmations []Confirmation `json:"confirmations"`
}

// ChainInfo describes the backend.
type ChainInfo struct {
	Backend     string            `json:"backend"` // "fisco-bcos" | "mock"
	BlockNumber int64             `json:"blockNumber"`
	ChainTime   int64             `json:"chainTime"`
	GroupID     int               `json:"groupId,omitempty"`
	ChainID     int64             `json:"chainId,omitempty"`
	Contracts   map[string]string `json:"contracts,omitempty"`
	Accounts    map[string]string `json:"accounts,omitempty"`
}
