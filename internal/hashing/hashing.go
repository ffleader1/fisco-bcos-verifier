// Package hashing turns off-chain data into the 32-byte values written on chain.
//
//   - Ref: salted HMAC-SHA256 references for withdrawal target accounts and staff ids, so no personal
//     identifier is ever written to the shared ledger (PIPL, 断直连 constraints), yet BuoyUp — who holds the
//     salt — can always recompute and match them.
//   - Content: SHA-256 over canonical JSON (sorted keys, no insignificant whitespace) for withdrawal records and
//     audit anchoring of algorithm outputs, agent traces, proposals and human decisions. Anyone holding the
//     document can re-hash and verify.
//   - Text: SHA-256 of a free-text reason / external reference.
package hashing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"buoyup/fisco/internal/ledger"
)

// Domains for Ref.
const (
	DomainAccount = "account" // userId + "|" + accountNo of a withdrawal target
	DomainStaff   = "staff"   // staff id of a human actor
)

// Ref returns HMAC-SHA256(salt, domain + ":" + id).
func Ref(salt []byte, domain, id string) ledger.Hash {
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte(domain + ":" + id))
	var h ledger.Hash
	copy(h[:], mac.Sum(nil))
	return h
}

// Canonical re-encodes a JSON document with sorted object keys and no whitespace. Numbers are kept verbatim.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("content is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("content has trailing data")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil { // encoding/json sorts map keys
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Content returns SHA-256 of the canonical form of a JSON document.
func Content(raw []byte) (ledger.Hash, error) {
	c, err := Canonical(raw)
	if err != nil {
		return ledger.Hash{}, err
	}
	return sha256.Sum256(c), nil
}

// ContentOf marshals v and hashes it canonically.
func ContentOf(v any) (ledger.Hash, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return ledger.Hash{}, err
	}
	return Content(raw)
}

// Text returns SHA-256 of a string (reasons, external references longer than 32 bytes).
func Text(s string) ledger.Hash { return sha256.Sum256([]byte(s)) }

// ExtRef encodes an external reference (bank transfer id, ticket no.): readable ASCII if it fits in
// 32 bytes, SHA-256 otherwise. Empty input yields the zero hash (rejected on chain).
func ExtRef(s string) ledger.Hash {
	var h ledger.Hash
	if s == "" {
		return h
	}
	if len(s) <= 32 && isPrintableASCII(s) {
		copy(h[:], s)
		return h
	}
	return Text(s)
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
