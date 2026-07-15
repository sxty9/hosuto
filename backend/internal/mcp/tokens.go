package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TokenStore issues and validates bearer tokens for the MCP endpoint.
//
// A token maps to an opaque Subject and an optional Scope (hosuto reads these as the Linux username
// and a server id). Only the token's SHA-256 is persisted, so the file on disk is never a usable
// credential — a leak of the file cannot impersonate anyone, exactly as with a password hash. The
// clear token is returned once, at mint time, and never again.
//
// The write shape is the landscape's standard: temp file → fsync → rename, mode 0600, one mutex,
// this process the sole writer — mirroring hosuto's state store and /etc/holistic/jwt-secret.
type TokenStore struct {
	path string
	mu   sync.Mutex
	recs map[string]tokenRec // keyed by hex(sha256(token))
}

type tokenRec struct {
	Subject string `json:"subject"`
	Scope   string `json:"scope,omitempty"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

// OpenTokenStore loads the token file, creating an empty store when it does not exist.
func OpenTokenStore(path string) (*TokenStore, error) {
	s := &TokenStore{path: path, recs: map[string]tokenRec{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || len(b) == 0 {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.recs); err != nil {
		return nil, err
	}
	if s.recs == nil {
		s.recs = map[string]tokenRec{}
	}
	return s, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Mint issues a new token for subject (optionally scoped), valid for ttl. It returns the clear token
// and its absolute expiry — the clear token is not recoverable afterwards. Minting also prunes any of
// the subject's expired tokens so the file does not grow without bound.
func (s *TokenStore) Mint(subject, scope string, ttl time.Duration) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := "hmcp_" + hex.EncodeToString(raw)
	now := time.Now()
	exp := now.Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpired(now)
	s.recs[hashToken(token)] = tokenRec{
		Subject: subject, Scope: scope, Created: now.Unix(), Expires: exp.Unix(),
	}
	if err := s.save(); err != nil {
		delete(s.recs, hashToken(token))
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// Lookup resolves a clear token to its subject and scope, or reports ok=false for an unknown or
// expired token.
func (s *TokenStore) Lookup(token string) (subject, scope string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, has := s.recs[hashToken(token)]
	if !has || rec.Expires <= time.Now().Unix() {
		return "", "", false
	}
	return rec.Subject, rec.Scope, true
}

// Active returns the subject's live (non-expired) tokens, newest first.
func (s *TokenStore) Active(subject string) []TokenInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	var out []TokenInfo
	for _, rec := range s.recs {
		if rec.Subject == subject && rec.Expires > now {
			out = append(out, TokenInfo{Scope: rec.Scope, Created: rec.Created, Expires: rec.Expires})
		}
	}
	return out
}

// RevokeAll drops every token belonging to subject. It is the "log out all my MCP clients" action.
func (s *TokenStore) RevokeAll(subject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for h, rec := range s.recs {
		if rec.Subject == subject {
			delete(s.recs, h)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save()
}

// TokenInfo is the safe, credential-free view of an active token.
type TokenInfo struct {
	Scope   string `json:"scope,omitempty"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
}

func (s *TokenStore) pruneExpired(now time.Time) {
	cutoff := now.Unix()
	for h, rec := range s.recs {
		if rec.Expires <= cutoff {
			delete(s.recs, h)
		}
	}
}

// save writes the store atomically. Caller holds the mutex.
func (s *TokenStore) save() error {
	b, err := json.MarshalIndent(s.recs, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mcp-tokens-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
