// Package fisco implements ledger.Ledger on FISCO BCOS 2.x (tested on v2.11.1) using the pure-Go
// github.com/FISCO-BCOS/go-sdk v1.1.1 (channel protocol, TLS with the chain's SDK certificates, ECDSA/secp256k1).
//
// Two keys sign: admin (deploy, role grants) and operator (the service: withdrawal records and audit anchors).
// The service can also connect to the other nodes of the chain (NodeConfig.Peers) to show, per transaction,
// which nodes already hold it, and the status of every node, in the explorer.
package fisco

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FISCO-BCOS/go-sdk/abi"
	"github.com/FISCO-BCOS/go-sdk/abi/bind"
	"github.com/FISCO-BCOS/go-sdk/client"
	"github.com/FISCO-BCOS/go-sdk/conf"
	"github.com/FISCO-BCOS/go-sdk/core/types"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"bouyup/fisco/contracts"
	"bouyup/fisco/internal/ledger"
)

// NodeConfig describes how to reach the chain over the channel port.
type NodeConfig struct {
	NodeURL  string   `json:"nodeURL"`         // ip:channel_port the service submits transactions to, e.g. 127.0.0.1:20200
	Peers    []string `json:"peers,omitempty"` // other nodes' channel ports (read-only), e.g. 127.0.0.1:20201..20203
	GroupID  int      `json:"groupId"`
	ChainID  int64    `json:"chainId"`
	CAFile   string   `json:"caFile"`   // nodes/<ip>/sdk/ca.crt
	CertFile string   `json:"certFile"` // nodes/<ip>/sdk/sdk.crt
	KeyFile  string   `json:"keyFile"`  // nodes/<ip>/sdk/sdk.key
}

// Keys holds the raw 32-byte secp256k1 private keys of the signing roles.
type Keys struct {
	Admin    []byte
	Operator []byte
}

// Addresses of the deployed contracts.
type Addresses struct {
	WithdrawalLedger string `json:"withdrawalLedger"`
	AuditTrail       string `json:"auditTrail"`
}

type nodeClient struct {
	name string
	url  string
	c    *client.Client
}

// Ledger is the FISCO BCOS implementation of ledger.Ledger and ledger.Admin.
type Ledger struct {
	node     NodeConfig
	addrs    Addresses
	accounts ledger.Accounts
	admin    *client.Client
	operator *client.Client
	nodes    []nodeClient // node0 = NodeURL (operator connection), then peers
	wdABI    abi.ABI
	auditABI abi.ABI
	wdAt     common.Address
	auditAt  common.Address
	mu       sync.Mutex // serialize writes (keeps event order deterministic for the PoC)

	cacheMu sync.Mutex
	txOf    map[string]string // withdrawal id -> tx hash

	commits commitLog // per-node block notices (step-by-step demo)
}

// ---------------------------------------------------------------- keys

// ParseKey accepts a 0x-hex private key, a file containing one, or a FISCO console PEM account file.
func ParseKey(hexOrFile string) ([]byte, error) {
	s := strings.TrimSpace(hexOrFile)
	if s == "" {
		return nil, fmt.Errorf("empty key")
	}
	if b, err := hex.DecodeString(strings.TrimPrefix(s, "0x")); err == nil && len(b) == 32 {
		return b, nil
	}
	raw, err := os.ReadFile(s)
	if err != nil {
		return nil, fmt.Errorf("key is neither 32-byte hex nor a readable file: %w", err)
	}
	if strings.Contains(string(raw), "BEGIN PRIVATE KEY") {
		k, curve, err := conf.LoadECPrivateKeyFromPEM(s)
		if err != nil {
			return nil, err
		}
		if curve != "secp256k1" {
			return nil, fmt.Errorf("%s: curve %s not supported (non-SM chain needs secp256k1)", s, curve)
		}
		return k, nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(string(raw)), "0x"))
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("%s: expected 32-byte hex private key", s)
	}
	return b, nil
}

// AddressOf returns the 0x address (lowercase) of a raw private key.
func AddressOf(key []byte) (string, error) {
	pk, err := crypto.ToECDSA(key)
	if err != nil {
		return "", err
	}
	return strings.ToLower(crypto.PubkeyToAddress(pk.PublicKey).Hex()), nil
}

// GenerateKey returns a new random secp256k1 key as raw bytes.
func GenerateKey() ([]byte, error) {
	pk, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	return crypto.FromECDSA(pk), nil
}

// AccountsOf derives role addresses from keys.
func AccountsOf(k Keys) (ledger.Accounts, error) {
	var acc ledger.Accounts
	var err error
	if k.Admin != nil {
		if acc.Admin, err = AddressOf(k.Admin); err != nil {
			return acc, fmt.Errorf("admin key: %w", err)
		}
	}
	if k.Operator != nil {
		if acc.Operator, err = AddressOf(k.Operator); err != nil {
			return acc, fmt.Errorf("operator key: %w", err)
		}
	}
	return acc, nil
}

// ---------------------------------------------------------------- dial / deploy

func dialKey(node NodeConfig, url string, key []byte) (*client.Client, error) {
	cfg := &conf.Config{
		IsHTTP: false, ChainID: node.ChainID, CAFile: node.CAFile, Cert: node.CertFile, Key: node.KeyFile,
		IsSMCrypto: false, GroupID: node.GroupID, NodeURL: url, PrivateKey: key,
	}
	c, err := client.Dial(cfg)
	if err != nil {
		return nil, fmt.Errorf("dial FISCO BCOS node %s (group %d): %w", url, node.GroupID, err)
	}
	return c, nil
}

// Deploy deploys WithdrawalLedger and AuditTrail with the admin key.
func Deploy(ctx context.Context, node NodeConfig, adminKey []byte) (Addresses, error) {
	c, err := dialKey(node, node.NodeURL, adminKey)
	if err != nil {
		return Addresses{}, err
	}
	defer c.Close()
	var out Addresses
	for _, d := range []struct {
		name, abiJSON, bin string
		dst                *string
	}{
		{"WithdrawalLedger", contracts.WithdrawalLedgerABI, contracts.WithdrawalLedgerBin, &out.WithdrawalLedger},
		{"AuditTrail", contracts.AuditTrailABI, contracts.AuditTrailBin, &out.AuditTrail},
	} {
		parsed, err := abi.JSON(strings.NewReader(d.abiJSON))
		if err != nil {
			return out, err
		}
		_, receipt, _, err := bind.DeployContractGetReceipt(c.GetTransactOpts(), parsed, common.FromHex(d.bin), c)
		if err != nil {
			return out, fmt.Errorf("deploy %s: %w", d.name, err)
		}
		if receipt.Status != types.Success {
			return out, fmt.Errorf("deploy %s failed: %s", d.name, receipt.GetErrorMessage())
		}
		*d.dst = strings.ToLower(receipt.ContractAddress.Hex())
	}
	return out, nil
}

// Dial connects the admin and operator keys to the node, opens read-only connections to the peers and binds the
// deployed contracts. An unreachable peer is not fatal: it shows as offline in the explorer.
func Dial(ctx context.Context, node NodeConfig, addrs Addresses, keys Keys) (*Ledger, error) {
	if keys.Operator == nil {
		return nil, fmt.Errorf("operator key is required")
	}
	acc, err := AccountsOf(keys)
	if err != nil {
		return nil, err
	}
	l := &Ledger{node: node, addrs: addrs, accounts: acc, txOf: map[string]string{},
		wdAt: common.HexToAddress(addrs.WithdrawalLedger), auditAt: common.HexToAddress(addrs.AuditTrail)}
	if l.wdABI, err = abi.JSON(strings.NewReader(contracts.WithdrawalLedgerABI)); err != nil {
		return nil, err
	}
	if l.auditABI, err = abi.JSON(strings.NewReader(contracts.AuditTrailABI)); err != nil {
		return nil, err
	}
	if l.operator, err = dialKey(node, node.NodeURL, keys.Operator); err != nil {
		return nil, err
	}
	l.nodes = append(l.nodes, nodeClient{name: "node0", url: node.NodeURL, c: l.operator})
	if keys.Admin != nil && !strings.EqualFold(acc.Admin, acc.Operator) {
		if l.admin, err = dialKey(node, node.NodeURL, keys.Admin); err != nil {
			l.Close()
			return nil, err
		}
	} else if keys.Admin != nil {
		l.admin = l.operator
	}
	for i, p := range node.Peers {
		p = strings.TrimSpace(p)
		if p == "" || p == node.NodeURL {
			continue
		}
		nc := nodeClient{name: fmt.Sprintf("node%d", i+1), url: p}
		if c, err := dialKey(node, p, keys.Operator); err == nil {
			nc.c = c
		}
		l.nodes = append(l.nodes, nc)
	}
	for name, at := range map[string]common.Address{"WithdrawalLedger": l.wdAt, "AuditTrail": l.auditAt} {
		code, err := l.operator.CodeAt(ctx, at, nil)
		if err != nil {
			l.Close()
			return nil, err
		}
		if len(code) == 0 || string(code) == "0x" {
			l.Close()
			return nil, fmt.Errorf("no %s contract at %s (run `sgchain deploy` first)", name, at.Hex())
		}
	}
	l.watchCommits()
	return l, nil
}

func (l *Ledger) Close() {
	seen := map[*client.Client]bool{}
	for _, c := range []*client.Client{l.admin, l.operator} {
		if c != nil && !seen[c] {
			seen[c] = true
			c.Close()
		}
	}
	for _, n := range l.nodes {
		if n.c != nil && !seen[n.c] {
			seen[n.c] = true
			n.c.Close()
		}
	}
}

func (l *Ledger) reader() *client.Client { return l.operator }

// ---------------------------------------------------------------- low level

const (
	cWithdrawal = "WithdrawalLedger"
	cAudit      = "AuditTrail"
)

func (l *Ledger) contract(name string) (abi.ABI, common.Address) {
	if name == cAudit {
		return l.auditABI, l.auditAt
	}
	return l.wdABI, l.wdAt
}

func (l *Ledger) transact(ctx context.Context, c *client.Client, contract, method string, args ...any) (ledger.Tx, error) {
	if c == nil {
		return ledger.Tx{}, ledger.Errorf(ledger.KindUnauthorized, "no signing key configured for %s", method)
	}
	parsed, at := l.contract(contract)
	l.mu.Lock()
	defer l.mu.Unlock()
	bc := bind.NewBoundContract(at, parsed, c, c, c)
	opts := *c.GetTransactOpts()
	opts.Context = ctx
	_, receipt, err := bc.Transact(&opts, method, args...)
	if err != nil {
		return ledger.Tx{}, ledger.Errorf(ledger.KindChain, "%s: %v", method, err)
	}
	if receipt.Status != types.Success {
		return ledger.Tx{}, revertError(receipt)
	}
	tx := ledger.Tx{Hash: receipt.TransactionHash, BlockNumber: hexInt(receipt.BlockNumber), BlockHash: receipt.BlockHash, Events: []ledger.Event{}}
	for _, lg := range receipt.Logs {
		if e, ok := decodeLog(lg, l.wdABI, l.auditABI); ok {
			tx.Events = append(tx.Events, e)
		}
	}
	return tx, nil
}

func hexInt(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"), 16, 64)
	return n
}

// revertError extracts the Solidity revert reason (Error(string)) from a failed receipt.
func revertError(r *types.Receipt) error {
	out := common.FromHex(r.Output)
	if len(out) >= 68 && hex.EncodeToString(out[:4]) == "08c379a0" {
		n := new(big.Int).SetBytes(out[36:68]).Int64()
		if 68+n <= int64(len(out)) {
			return ledger.FromRevert(string(out[68 : 68+n]))
		}
	}
	return ledger.Errorf(ledger.KindChain, "%s", r.GetErrorMessage())
}

func (l *Ledger) call(ctx context.Context, contract, method string, args ...any) ([]any, error) {
	parsed, at := l.contract(contract)
	input, err := parsed.Pack(method, args...)
	if err != nil {
		return nil, ledger.Errorf(ledger.KindInvalid, "pack %s: %v", method, err)
	}
	c := l.reader()
	out, err := c.CallContract(ctx, ethereum.CallMsg{From: c.GetCallOpts().From, To: &at, Data: input}, nil)
	if err != nil {
		msg := err.Error()
		if i := strings.Index(msg, "E_"); i >= 0 {
			return nil, ledger.FromRevert(strings.TrimRight(msg[i:], "\x00"))
		}
		return nil, ledger.Errorf(ledger.KindChain, "%s: %v", method, err)
	}
	return parsed.Methods[method].Outputs.UnpackValues(out)
}

// ---------------------------------------------------------------- type helpers

func b32(h ledger.Hash) [32]byte { return [32]byte(h) }
func bi(v int64) *big.Int        { return big.NewInt(v) }
func i64(v any) int64 {
	switch x := v.(type) {
	case *big.Int:
		return x.Int64()
	case uint8:
		return int64(x)
	case uint64:
		return int64(x)
	}
	panic(fmt.Sprintf("unexpected numeric type %T", v))
}
func h32(v any) ledger.Hash { return ledger.Hash(v.([32]byte)) }

func idArg(id string) ([32]byte, error) {
	h, err := ledger.IDToBytes32(id)
	return [32]byte(h), err
}

func textArg(field, v string) ([32]byte, error) {
	h, err := ledger.TextToBytes32(field, v)
	return [32]byte(h), err
}

// ---------------------------------------------------------------- ledger.Admin

// GrantRole grants OPERATOR on WithdrawalLedger and AUDIT_WRITER on AuditTrail (each contract has its own registry).
func (l *Ledger) GrantRole(ctx context.Context, account string, role uint64) (ledger.Tx, error) {
	var last ledger.Tx
	if role&ledger.RoleAuditWriter != 0 {
		tx, err := l.transact(ctx, l.admin, cAudit, "grantRole", common.HexToAddress(account), new(big.Int).SetUint64(ledger.RoleAuditWriter))
		if err != nil {
			return tx, err
		}
		last = tx
	}
	if rest := role &^ ledger.RoleAuditWriter; rest != 0 {
		return l.transact(ctx, l.admin, cWithdrawal, "grantRole", common.HexToAddress(account), new(big.Int).SetUint64(rest))
	}
	return last, nil
}

// HasRole checks AUDIT_WRITER on AuditTrail and the other bits on WithdrawalLedger.
func (l *Ledger) HasRole(ctx context.Context, account string, role uint64) (bool, error) {
	check := func(contract string, bits uint64) (bool, error) {
		v, err := l.call(ctx, contract, "hasRole", common.HexToAddress(account), new(big.Int).SetUint64(bits))
		if err != nil {
			return false, err
		}
		return v[0].(bool), nil
	}
	if role&ledger.RoleAuditWriter != 0 {
		if ok, err := check(cAudit, ledger.RoleAuditWriter); err != nil || !ok {
			return false, err
		}
	}
	if rest := role &^ ledger.RoleAuditWriter; rest != 0 {
		return check(cWithdrawal, rest)
	}
	return true, nil
}

// ---------------------------------------------------------------- info

func (l *Ledger) Info(ctx context.Context) (ledger.ChainInfo, error) {
	bn, err := l.reader().GetBlockNumber(ctx)
	if err != nil {
		return ledger.ChainInfo{}, ledger.Errorf(ledger.KindChain, "getBlockNumber: %v", err)
	}
	now, err := l.Now(ctx)
	if err != nil {
		return ledger.ChainInfo{}, err
	}
	return ledger.ChainInfo{Backend: "fisco-bcos", BlockNumber: bn, ChainTime: now, GroupID: l.node.GroupID, ChainID: l.node.ChainID,
		Accounts:  map[string]string{"admin": l.accounts.Admin, "operator": l.accounts.Operator},
		Contracts: map[string]string{"WithdrawalLedger": l.addrs.WithdrawalLedger, "AuditTrail": l.addrs.AuditTrail}}, nil
}

// Now returns the latest block timestamp in seconds (FISCO block timestamps are milliseconds).
func (l *Ledger) Now(ctx context.Context) (int64, error) {
	c := l.reader()
	bn, err := c.GetBlockNumber(ctx)
	if err != nil {
		return 0, ledger.Errorf(ledger.KindChain, "getBlockNumber: %v", err)
	}
	if bn == 0 {
		return time.Now().Unix(), nil
	}
	b, err := c.GetBlockByNumber(ctx, bn, false)
	if err != nil {
		return 0, ledger.Errorf(ledger.KindChain, "getBlockByNumber: %v", err)
	}
	return hexInt(b.Timestamp) / 1000, nil
}

var _ ledger.Ledger = (*Ledger)(nil)
var _ ledger.Admin = (*Ledger)(nil)
