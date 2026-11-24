// Package auth holds the secrets of a control plane and checks callers
// against them.
//
// On first start the control plane creates, in its data directory:
//
//   - a certificate authority of its own, and a server certificate signed by it
//   - three tokens: one for administrators, one that may only allocate, and
//     one that lets a node join
//
// Every API is served over TLS with that certificate, and every call must
// carry one of the tokens. A client that cannot be given the CA certificate
// up front (an agent joining from another machine) instead pins the CA by
// its fingerprint, which is part of the join token.
//
//   - auth.go: loading and creating the secrets
//   - tls.go: the certificates and the pinning
//   - check.go: checking tokens on HTTP and gRPC calls
//
// Upstream counterpart: none in one place. Agones relies on Kubernetes RBAC,
// and its allocator service on mutual TLS.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Role is what a token allows.
type Role string

// The three roles. Admin may do everything the other two may.
const (
	Admin     Role = "admin"
	Allocator Role = "allocator"
	Node      Role = "join"
)

var roles = []Role{Admin, Allocator, Node}

// Secrets is everything in the data directory that must stay private.
type Secrets struct {
	dir    string
	ca     *x509.Certificate
	server tls.Certificate

	mu     sync.RWMutex
	tokens map[Role]string
}

// Load reads the secrets under dir and creates whatever is missing. names
// are the extra host names and addresses the server certificate must cover.
func Load(dir string, names []string) (*Secrets, error) {
	s := &Secrets{dir: dir, tokens: map[Role]string{}}
	if err := os.MkdirAll(filepath.Join(dir, "tokens"), 0o700); err != nil {
		return nil, err
	}
	if err := s.loadCertificates(names); err != nil {
		return nil, err
	}
	return s, s.Reload()
}

// Reload reads the tokens from disk again, creating any that are missing.
// A running control plane calls it now and then, so that a token rotated
// with the command line takes effect without a restart.
func (s *Secrets) Reload() error {
	for _, role := range roles {
		token, err := loadOrCreate(s.tokenFile(role), newToken)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.tokens[role] = strings.TrimSpace(string(token))
		s.mu.Unlock()
	}
	return nil
}

func (s *Secrets) tokenFile(role Role) string {
	return filepath.Join(s.dir, "tokens", string(role))
}

// Token returns the token of a role, as an operator would hand it out. The
// join token is prefixed with the CA's fingerprint so that a new node can
// tell the real control plane from an impostor.
func (s *Secrets) Token(role Role) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if role == Node {
		return Fingerprint(s.ca) + "." + s.tokens[Node]
	}
	return s.tokens[role]
}

// Rotate replaces the token of a role. The old one stops working at once.
func (s *Secrets) Rotate(role Role) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.tokenFile(role), token, 0o600); err != nil {
		return err
	}
	return s.Reload()
}

// Allows reports whether token grants what role may do.
func (s *Secrets) Allows(role Role, token string) bool {
	if _, secret, pinned := strings.Cut(token, "."); pinned {
		token = secret // a join token carries the CA fingerprint in front
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return same(token, s.tokens[role]) || same(token, s.tokens[Admin])
}

// same compares in constant time, so the answer leaks nothing about how
// much of a guess was right.
func same(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ParseRole turns a name typed by an operator into a Role.
func ParseRole(name string) (Role, error) {
	for _, role := range roles {
		if string(role) == name {
			return role, nil
		}
	}
	return "", fmt.Errorf("unknown token %q: want admin, allocator or join", name)
}

func newToken() ([]byte, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(raw)), nil
}

// loadOrCreate reads a secret file, or creates it with what create returns.
func loadOrCreate(path string, create func() ([]byte, error)) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err = create()
	if err != nil {
		return nil, err
	}
	return data, os.WriteFile(path, data, 0o600)
}
