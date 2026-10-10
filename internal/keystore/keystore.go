// Package keystore holds the encryption keys of the withdrawal envelopes: the BouyUp master key pair and one
// public key per bank (plus, in this demo, each bank's private key so the team can test decryption).
//
// In production a bank keeps its private key itself and only registers its public key with BouyUp; changing
// it is the bank's business (PUT /v1/banks/{code}/public-key). BouyUp only needs each bank's CURRENT public
// key: new withdrawals are locked to it, old ones stay locked to whatever key was current then (the
// withdrawal's timestamp tells the bank which of its keys to try).
//
// Keys live in one JSON file (config "bankKeysFile"), created with fresh demo keys on first start.
package keystore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"bouyup/fisco/internal/sealing"
)

// Key is one key pair with its history.
type Key struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey,omitempty"` // demo only; absent when a bank registered its own key
	ActiveFrom string `json:"activeFrom"`           // RFC3339
	Retired    []Old  `json:"retired,omitempty"`    // earlier keys, newest first (kept so demo records stay readable)
}

// Old is a key that was replaced.
type Old struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey,omitempty"`
	ActiveFrom string `json:"activeFrom"`
	RetiredAt  string `json:"retiredAt"`
}

type file struct {
	Note   string          `json:"note"`
	Master Key             `json:"master"`
	Banks  map[string]*Key `json:"banks"`
}

// Store is the loaded key file.
type Store struct {
	mu   sync.Mutex
	path string // "" = in memory only
	f    file
}

const note = "BouyUp DEMO KEYS — test only. X25519 key pairs (hex). In production banks keep their private keys and BouyUp keeps the master key in an HSM/KMS."

// Open loads path (creating it with fresh keys when missing) and makes sure every bank code has a key.
// path "" keeps the keys in memory.
func Open(path string, bankCodes []string, now time.Time) (*Store, error) {
	s := &Store{path: path, f: file{Note: note, Banks: map[string]*Key{}}}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &s.f); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if s.f.Banks == nil {
				s.f.Banks = map[string]*Key{}
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, err
		}
	}
	changed := false
	gen := func() (Key, error) {
		kp, err := sealing.NewKeyPair()
		return Key{PublicKey: kp.Public, PrivateKey: kp.Private, ActiveFrom: now.UTC().Format(time.RFC3339)}, err
	}
	if s.f.Master.PublicKey == "" {
		k, err := gen()
		if err != nil {
			return nil, err
		}
		s.f.Master, changed = k, true
	}
	for _, c := range bankCodes {
		if s.f.Banks[c] == nil {
			k, err := gen()
			if err != nil {
				return nil, err
			}
			s.f.Banks[c], changed = &k, true
		}
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	if changed {
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) check() error {
	if s.f.Master.PrivateKey == "" {
		return errors.New("keystore: the master private key is required")
	}
	if pub, err := sealing.PublicFromPrivate(s.f.Master.PrivateKey); err != nil || !strings.EqualFold(pub, s.f.Master.PublicKey) {
		return errors.New("keystore: master public key does not match its private key")
	}
	for code, k := range s.f.Banks {
		if _, err := sealing.ParsePublic(k.PublicKey); err != nil {
			return fmt.Errorf("keystore: bank %s: %w", code, err)
		}
	}
	return nil
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(s.f, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Path is where the keys are stored ("" = memory).
func (s *Store) Path() string { return s.path }

// MasterPrivate returns the BouyUp master private key (hex).
func (s *Store) MasterPrivate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Master.PrivateKey
}

// MasterPublic returns the BouyUp master public key (hex).
func (s *Store) MasterPublic() string { s.mu.Lock(); defer s.mu.Unlock(); return s.f.Master.PublicKey }

// BankPublic returns a bank's current public key.
func (s *Store) BankPublic(code string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.f.Banks[code]
	if !ok {
		return "", false
	}
	return k.PublicKey, true
}

// Bank returns a copy of a bank's key record.
func (s *Store) Bank(code string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.f.Banks[code]
	if !ok {
		return Key{}, false
	}
	c := *k
	c.Retired = append([]Old(nil), k.Retired...)
	return c, true
}

// Master returns a copy of the master key record.
func (s *Store) Master() Key { s.mu.Lock(); defer s.mu.Unlock(); return s.f.Master }

// Codes lists the banks with keys, sorted.
func (s *Store) Codes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.f.Banks))
	for c := range s.f.Banks {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// SetBankPublic registers a bank's new public key. The previous key moves to Retired. Registering the key that
// is already current changes nothing.
func (s *Store) SetBankPublic(code, pub string, now time.Time) (Key, error) {
	if _, err := sealing.ParsePublic(pub); err != nil {
		return Key{}, err
	}
	pub = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(pub), "0x"))
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.f.Banks[code]
	if !ok {
		return Key{}, fmt.Errorf("no key record for bank %s", code)
	}
	if strings.EqualFold(k.PublicKey, pub) {
		return *k, nil
	}
	ts := now.UTC().Format(time.RFC3339)
	k.Retired = append([]Old{{PublicKey: k.PublicKey, PrivateKey: k.PrivateKey, ActiveFrom: k.ActiveFrom, RetiredAt: ts}}, k.Retired...)
	k.PublicKey, k.PrivateKey, k.ActiveFrom = pub, "", ts
	if err := s.save(); err != nil {
		return Key{}, err
	}
	return *k, nil
}
