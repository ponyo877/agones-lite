package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caLifetime     = 10 * 365 * 24 * time.Hour
	serverLifetime = 2 * 365 * 24 * time.Hour
)

// loadCertificates makes sure there is a CA and a server certificate that
// covers names, creating either when needed.
func (s *Secrets) loadCertificates(names []string) error {
	tlsDir := filepath.Join(s.dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		return err
	}
	caCert, caKey, err := loadOrCreatePair(filepath.Join(tlsDir, "ca"), func() ([]byte, *ecdsa.PrivateKey, error) {
		return selfSign(caTemplate())
	})
	if err != nil {
		return err
	}
	s.ca = caCert

	template := serverTemplate(names)
	serverPath := filepath.Join(tlsDir, "server")
	if existing, _, err := readPair(serverPath); err == nil && !covers(existing, template) {
		_ = os.Remove(serverPath + ".crt") // the names changed: issue a new one
	}
	leaf, key, err := loadOrCreatePair(serverPath, func() ([]byte, *ecdsa.PrivateKey, error) {
		return sign(template, caCert, caKey)
	})
	if err != nil {
		return err
	}
	// The CA goes along, so that a client that pins it can find it.
	s.server = tls.Certificate{Certificate: [][]byte{leaf.Raw, caCert.Raw}, PrivateKey: key, Leaf: leaf}
	return nil
}

// ServerTLS returns the configuration every listener of the control plane uses.
func (s *Secrets) ServerTLS() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{s.server}, MinVersion: tls.VersionTLS12}
}

// ServerTLSWithClients is ServerTLS for a listener that also accepts client
// certificates signed by one of the CAs in dir. It is how Agones' allocator
// service authenticates, so matchmakers set up for Agones keep working.
func (s *Secrets) ServerTLSWithClients(dir string) (*tls.Config, error) {
	config := s.ServerTLS()
	if dir == "" {
		return config, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, file := range files {
		if data, err := os.ReadFile(file); err == nil {
			pool.AppendCertsFromPEM(data)
		}
	}
	config.ClientCAs = pool
	config.ClientAuth = tls.VerifyClientCertIfGiven // a token is the other way in
	return config, nil
}

// CAFile returns where the CA certificate is, for clients on this machine.
func (s *Secrets) CAFile() string {
	return filepath.Join(s.dir, "tls", "ca.crt")
}

// Fingerprint identifies a certificate: the SHA-256 of its encoded form.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// TrustFile returns a client configuration that trusts the CA in a file.
func TrustFile(caFile string) (*tls.Config, error) {
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New(caFile + " holds no certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// TrustPinned returns a client configuration that trusts whichever server
// presents a CA with the given fingerprint and a certificate signed by it.
// The host name is not checked: the fingerprint already says who it is.
func TrustPinned(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // replaced by the check below
		VerifyPeerCertificate: func(chain [][]byte, _ [][]*x509.Certificate) error {
			return verifyPinned(chain, fingerprint)
		},
	}
}

func verifyPinned(chain [][]byte, fingerprint string) error {
	if len(chain) == 0 {
		return errors.New("the server presented no certificate")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return err
	}
	for _, raw := range chain[1:] {
		ca, err := x509.ParseCertificate(raw)
		if err != nil || Fingerprint(ca) != fingerprint {
			continue
		}
		return leaf.CheckSignatureFrom(ca)
	}
	return errors.New("the server's certificate authority does not match the join token")
}

func caTemplate() *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "agones-lite CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
}

// serverTemplate covers this machine's usual names plus the ones given.
func serverTemplate(names []string) *x509.Certificate {
	template := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "agones-lite"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	hostname, _ := os.Hostname()
	for _, name := range append([]string{"localhost", "127.0.0.1", "::1", hostname}, names...) {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if name != "" {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	return template
}

// covers reports whether cert is still good for every name in wanted.
func covers(cert, wanted *x509.Certificate) bool {
	if time.Now().After(cert.NotAfter) {
		return false
	}
	for _, name := range wanted.DNSNames {
		if cert.VerifyHostname(name) != nil {
			return false
		}
	}
	for _, ip := range wanted.IPAddresses {
		if cert.VerifyHostname(ip.String()) != nil {
			return false
		}
	}
	return true
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func selfSign(template *x509.Certificate) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	return der, key, err
}

func sign(template, ca *x509.Certificate, caKey *ecdsa.PrivateKey) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	return der, key, err
}

// loadOrCreatePair reads base.crt and base.key, creating both when the
// certificate is missing.
func loadOrCreatePair(base string, create func() ([]byte, *ecdsa.PrivateKey, error)) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	if cert, key, err := readPair(base); err == nil {
		return cert, key, nil
	}
	der, key, err := create()
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(base+".key", "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, nil, err
	}
	if err := writePEM(base+".crt", "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func readPair(base string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certDER, err := readPEM(base + ".crt")
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := readPEM(base + ".key")
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	return cert, key, err
}

func readPEM(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New(path + " is not PEM")
	}
	return block.Bytes, nil
}

func writePEM(path, kind string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), mode)
}
