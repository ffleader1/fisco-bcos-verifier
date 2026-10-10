// Package sealing encrypts a withdrawal so that each party can read only its own part, and commits to every
// part so nobody can change it afterwards.
//
// A withdrawal is split into pieces: piece 0 is the summary (amount, region, account reference, actor…),
// then one piece per bank (that bank's percent and amount). Each piece is encrypted with its own random
// 256-bit key (AES-256-GCM). The piece keys are then handed out in "slots": one slot per reader, encrypted
// with a key only that reader can derive (X25519 key agreement + HKDF-SHA256). A bank's slot holds the keys of
// the summary and of its own piece; the BuoyUp master slot holds every key. Slots carry no names and are
// shuffled: a reader finds its slot by trying its key (AES-GCM fails cleanly on the wrong one).
//
// Commitments: SHA-256 of each piece's exact plaintext bytes (each plaintext carries a random salt, so a
// commitment cannot be guessed from likely values). The Merkle root of the commitments goes on chain with the
// commitments and the encrypted envelope; the contract recomputes the root itself.
//
// Envelope format (JSON, version 1) — also implemented by scripts/decrypt_withdrawal.py:
//
//	{"v":1, "alg":"X25519-HKDF-SHA256/AES-256-GCM", "eph":"<hex 32-byte ephemeral public key>",
//	 "pieces":[{"nonce":"<hex 12>","ct":"<base64 ciphertext+tag>"}, …],
//	 "slots": [{"nonce":"<hex 12>","ct":"<base64 ciphertext+tag>"}, …]}
//
//	slot key  = HKDF-SHA256(ikm = X25519(reader private, eph), salt = eph || reader public,
//	                         info = "BouyUp withdrawal v1 " + withdrawalId, length 32)
//	slot      = AES-256-GCM(slot key, nonce, {"keys":[{"piece":i,"key":"<hex 32>"},…]}, aad = withdrawalId + "|slot")
//	piece i   = AES-256-GCM(piece key i, nonce, plaintext JSON, aad = withdrawalId + "|" + i)
//	commitment i = SHA-256(plaintext i)
package sealing

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand"
	"strconv"
	"strings"

	"buoyup/fisco/internal/ledger"
)

// Algorithm names the scheme recorded in every envelope.
const Algorithm = "X25519-HKDF-SHA256/AES-256-GCM"

// ---------------------------------------------------------------- keys

// KeyPair is an X25519 key pair, hex-encoded (32 bytes each).
type KeyPair struct {
	Private string `json:"privateKey,omitempty"`
	Public  string `json:"publicKey"`
}

// NewKeyPair creates a random X25519 key pair.
func NewKeyPair() (KeyPair, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Private: hex.EncodeToString(k.Bytes()), Public: hex.EncodeToString(k.PublicKey().Bytes())}, nil
}

// PublicFromPrivate derives the public key of a hex private key.
func PublicFromPrivate(privHex string) (string, error) {
	k, err := parsePrivate(privHex)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k.PublicKey().Bytes()), nil
}

func parsePrivate(s string) (*ecdh.PrivateKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(b) != 32 {
		return nil, errors.New("private key must be 32 bytes of hex (64 characters)")
	}
	return ecdh.X25519().NewPrivateKey(b)
}

// ParsePublic checks a hex X25519 public key.
func ParsePublic(s string) (*ecdh.PublicKey, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if err != nil || len(b) != 32 {
		return nil, errors.New("public key must be 32 bytes of hex (64 characters)")
	}
	return ecdh.X25519().NewPublicKey(b)
}

// ---------------------------------------------------------------- envelope

// Box is one encrypted blob.
type Box struct {
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

// Envelope is what goes on chain.
type Envelope struct {
	V      int    `json:"v"`
	Alg    string `json:"alg"`
	Eph    string `json:"eph"`
	Pieces []Box  `json:"pieces"`
	Slots  []Box  `json:"slots"`
}

// Piece is one plaintext and who may read it (indexes into the recipients passed to Seal).
type Piece struct {
	Plain   []byte
	Readers []int
}

// Sealed is the result of Seal.
type Sealed struct {
	Envelope    Envelope
	Raw         []byte // canonical JSON bytes of Envelope (what goes on chain)
	Commitments []ledger.Hash
	Root        ledger.Hash
}

// HKDF-SHA256 for a 32-byte output (RFC 5869, one block).
func hkdf32(ikm, salt, info []byte) []byte {
	ext := hmac.New(sha256.New, salt)
	ext.Write(ikm)
	prk := ext.Sum(nil)
	exp := hmac.New(sha256.New, prk)
	exp.Write(info)
	exp.Write([]byte{1})
	return exp.Sum(nil)
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func seal(key, plain, aad []byte) (Box, error) {
	a, err := gcm(key)
	if err != nil {
		return Box{}, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return Box{}, err
	}
	return Box{Nonce: hex.EncodeToString(nonce), CT: base64.StdEncoding.EncodeToString(a.Seal(nil, nonce, plain, aad))}, nil
}

func open(key []byte, b Box, aad []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(b.Nonce)
	if err != nil || len(nonce) != a.NonceSize() {
		return nil, errors.New("bad nonce")
	}
	ct, err := base64.StdEncoding.DecodeString(b.CT)
	if err != nil {
		return nil, errors.New("bad ciphertext encoding")
	}
	return a.Open(nil, nonce, ct, aad)
}

// info is the HKDF info label. Frozen identifier: keeps the old "BouyUp" spelling on purpose. Changing it would make every withdrawal already on the chain unreadable.
func info(id string) []byte    { return []byte("BouyUp withdrawal v1 " + id) }
func slotAAD(id string) []byte { return []byte(id + "|slot") }
func pieceAAD(id string, i int) []byte {
	return []byte(id + "|" + strconv.Itoa(i))
}

func slotKey(eph *ecdh.PrivateKey, ephPub, readerPub *ecdh.PublicKey, id string) ([]byte, error) {
	shared, err := eph.ECDH(readerPub)
	if err != nil {
		return nil, err
	}
	return hkdf32(shared, append(append([]byte{}, ephPub.Bytes()...), readerPub.Bytes()...), info(id)), nil
}

// Seal encrypts the pieces for the recipients (hex public keys). Every recipient gets one slot holding the keys
// of the pieces it may read.
func Seal(id string, recipients []string, pieces []Piece) (Sealed, error) {
	if len(pieces) == 0 || len(recipients) == 0 {
		return Sealed{}, errors.New("nothing to seal")
	}
	pubs := make([]*ecdh.PublicKey, len(recipients))
	for i, r := range recipients {
		p, err := ParsePublic(r)
		if err != nil {
			return Sealed{}, fmt.Errorf("recipient %d: %w", i, err)
		}
		pubs[i] = p
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return Sealed{}, err
	}
	env := Envelope{V: 1, Alg: Algorithm, Eph: hex.EncodeToString(eph.PublicKey().Bytes())}
	var out Sealed
	keys := make([][]byte, len(pieces))
	for i, pc := range pieces {
		keys[i] = make([]byte, 32)
		if _, err := rand.Read(keys[i]); err != nil {
			return Sealed{}, err
		}
		b, err := seal(keys[i], pc.Plain, pieceAAD(id, i))
		if err != nil {
			return Sealed{}, err
		}
		env.Pieces = append(env.Pieces, b)
		out.Commitments = append(out.Commitments, ledger.Hash(sha256.Sum256(pc.Plain)))
	}
	type keyRef struct {
		Piece int    `json:"piece"`
		Key   string `json:"key"`
	}
	for r := range recipients {
		var refs []keyRef
		for i, pc := range pieces {
			for _, rd := range pc.Readers {
				if rd == r {
					refs = append(refs, keyRef{Piece: i, Key: hex.EncodeToString(keys[i])})
				}
			}
		}
		if len(refs) == 0 {
			continue
		}
		plain, _ := json.Marshal(map[string]any{"keys": refs})
		k, err := slotKey(eph, eph.PublicKey(), pubs[r], id)
		if err != nil {
			return Sealed{}, err
		}
		b, err := seal(k, plain, slotAAD(id))
		if err != nil {
			return Sealed{}, err
		}
		env.Slots = append(env.Slots, b)
	}
	mrand.Shuffle(len(env.Slots), func(i, j int) { env.Slots[i], env.Slots[j] = env.Slots[j], env.Slots[i] })
	out.Envelope = env
	out.Raw, _ = json.Marshal(env)
	out.Root = ledger.MerkleRoot(out.Commitments)
	return out, nil
}

// Opened is one piece a key could read.
type Opened struct {
	Index        int             `json:"index"`
	Plaintext    json.RawMessage `json:"plaintext"`
	Commitment   ledger.Hash     `json:"commitment"`
	CommitmentOK bool            `json:"commitmentMatches"` // SHA-256(plaintext) equals the on-chain commitment
}

// ErrNoSlot means the key opens no slot of this envelope: the holder may not read this withdrawal.
var ErrNoSlot = errors.New("this key opens no slot of the withdrawal")

// Open decrypts what the private key may read. Commitments (from the chain) are checked when given.
func Open(id string, raw []byte, privHex string, commitments []ledger.Hash) ([]Opened, error) {
	priv, err := parsePrivate(privHex)
	if err != nil {
		return nil, err
	}
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("envelope is not valid JSON: %w", err)
	}
	if env.V != 1 || env.Alg != Algorithm {
		return nil, fmt.Errorf("unsupported envelope v%d %q", env.V, env.Alg)
	}
	ephPub, err := ParsePublic(env.Eph)
	if err != nil {
		return nil, fmt.Errorf("envelope eph: %w", err)
	}
	shared, err := priv.ECDH(ephPub)
	if err != nil {
		return nil, err
	}
	k := hkdf32(shared, append(append([]byte{}, ephPub.Bytes()...), priv.PublicKey().Bytes()...), info(id))
	for _, s := range env.Slots {
		plain, err := open(k, s, slotAAD(id))
		if err != nil {
			continue // not this reader's slot
		}
		var slot struct {
			Keys []struct {
				Piece int    `json:"piece"`
				Key   string `json:"key"`
			} `json:"keys"`
		}
		if err := json.Unmarshal(plain, &slot); err != nil {
			return nil, fmt.Errorf("slot content: %w", err)
		}
		var out []Opened
		for _, ref := range slot.Keys {
			if ref.Piece < 0 || ref.Piece >= len(env.Pieces) {
				return nil, fmt.Errorf("slot names piece %d, envelope has %d", ref.Piece, len(env.Pieces))
			}
			key, err := hex.DecodeString(ref.Key)
			if err != nil {
				return nil, err
			}
			p, err := open(key, env.Pieces[ref.Piece], pieceAAD(id, ref.Piece))
			if err != nil {
				return nil, fmt.Errorf("piece %d does not decrypt: %w", ref.Piece, err)
			}
			o := Opened{Index: ref.Piece, Plaintext: p, Commitment: ledger.Hash(sha256.Sum256(p))}
			if ref.Piece < len(commitments) {
				o.CommitmentOK = commitments[ref.Piece] == o.Commitment
			}
			out = append(out, o)
		}
		return out, nil
	}
	return nil, ErrNoSlot
}

// Counts reports how many pieces and slots an envelope holds, without decrypting.
func Counts(raw []byte) (pieces, slots int, err error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, 0, err
	}
	return len(env.Pieces), len(env.Slots), nil
}

// Salt returns 16 random bytes as hex, mixed into every plaintext so commitments cannot be guessed.
func Salt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
