package auth

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretsAreCreatedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir, nil)
	require.NoError(t, err)
	second, err := Load(dir, nil)
	require.NoError(t, err)

	for _, role := range roles {
		assert.NotEmpty(t, first.Token(role))
		assert.Equal(t, first.Token(role), second.Token(role), "a restart must not change the %s token", role)
	}
	assert.Equal(t, Fingerprint(first.ca), Fingerprint(second.ca))
	assert.Equal(t, first.server.Leaf.SerialNumber, second.server.Leaf.SerialNumber)

	info, err := os.Stat(filepath.Join(dir, "tokens", "admin"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "tokens are for the owner only")
}

func TestWhatEachTokenAllows(t *testing.T) {
	s, err := Load(t.TempDir(), nil)
	require.NoError(t, err)
	admin, allocator, join := s.Token(Admin), s.Token(Allocator), s.Token(Node)

	assert.True(t, s.Allows(Admin, admin))
	assert.True(t, s.Allows(Allocator, admin), "the admin token may do everything")
	assert.True(t, s.Allows(Node, admin))

	assert.True(t, s.Allows(Allocator, allocator))
	assert.False(t, s.Allows(Admin, allocator), "the allocator token may only allocate")

	assert.True(t, s.Allows(Node, join), "the fingerprint in front is not part of the secret")
	assert.False(t, s.Allows(Admin, join))

	assert.False(t, s.Allows(Admin, ""))
	assert.False(t, s.Allows(Admin, "wrong"))
}

func TestRotateRetiresTheOldToken(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir, nil)
	require.NoError(t, err)
	old := s.Token(Allocator)

	require.NoError(t, s.Rotate(Allocator))

	assert.False(t, s.Allows(Allocator, old))
	assert.True(t, s.Allows(Allocator, s.Token(Allocator)))
	reloaded, err := Load(dir, nil)
	require.NoError(t, err)
	assert.Equal(t, s.Token(Allocator), reloaded.Token(Allocator))
}

func TestTheServerCertificateFollowsTheNames(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir, []string{"cp.example.com"})
	require.NoError(t, err)
	assert.NoError(t, first.server.Leaf.VerifyHostname("cp.example.com"))
	assert.NoError(t, first.server.Leaf.VerifyHostname("127.0.0.1"))

	more, err := Load(dir, []string{"cp.example.com", "203.0.113.10"})
	require.NoError(t, err)
	assert.NoError(t, more.server.Leaf.VerifyHostname("203.0.113.10"))
	assert.NotEqual(t, first.server.Leaf.SerialNumber, more.server.Leaf.SerialNumber, "a new name needs a new certificate")
	assert.Equal(t, Fingerprint(first.ca), Fingerprint(more.ca), "the CA stays, so pinned clients keep working")
}

// serve starts an HTTPS server with the secrets and returns its address.
func serve(t *testing.T, s *Secrets) string {
	t.Helper()
	handler := RequireHTTP(s, Admin, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server := httptest.NewUnstartedServer(handler)
	server.TLS = s.ServerTLS()
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL
}

func get(t *testing.T, url string, config *tls.Config, token string) (int, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, nil
}

func TestClientsCanTrustTheCAByFileOrByFingerprint(t *testing.T) {
	s, err := Load(t.TempDir(), nil)
	require.NoError(t, err)
	url := serve(t, s)

	byFile, err := TrustFile(s.CAFile())
	require.NoError(t, err)
	code, err := get(t, url, byFile, s.Token(Admin))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)

	code, err = get(t, url, TrustPinned(Fingerprint(s.ca)), s.Token(Admin))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, code)

	other, err := Load(t.TempDir(), nil)
	require.NoError(t, err)
	_, err = get(t, url, TrustPinned(Fingerprint(other.ca)), s.Token(Admin))
	assert.Error(t, err, "a server with another CA must be refused")
}

func TestRequestsWithoutAGoodTokenAreTurnedAway(t *testing.T) {
	s, err := Load(t.TempDir(), nil)
	require.NoError(t, err)
	url := serve(t, s)
	trust, err := TrustFile(s.CAFile())
	require.NoError(t, err)

	for name, token := range map[string]string{"no token": "", "a wrong token": "nope", "a token for less": s.Token(Allocator)} {
		code, err := get(t, url, trust, token)
		require.NoError(t, err, name)
		assert.Equal(t, http.StatusUnauthorized, code, name)
	}
}

func TestParseRole(t *testing.T) {
	role, err := ParseRole("join")
	require.NoError(t, err)
	assert.Equal(t, Node, role)

	_, err = ParseRole("root")
	assert.Error(t, err)
}
