package auth

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

// flip is a Checker whose answer can be changed, like a token being rotated.
type flip struct{ allow atomic.Bool }

func (f *flip) Allows(Role, string) bool { return f.allow.Load() }

// openStream is the server's side of a stream that carries a token.
type openStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s openStream) Context() context.Context { return s.ctx }

func TestAnOpenStreamEndsWhenItsTokenStopsBeingValid(t *testing.T) {
	check := &flip{}
	check.allow.Store(true)
	guard := guardStream(func(ctx context.Context) error { return allowedGRPC(ctx, check, Node) }, 10*time.Millisecond)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer old-token"))
	started := make(chan struct{})
	// The handler is a stream that stays open, as an agent's Watch does.
	handler := func(_ any, ss grpc.ServerStream) error {
		close(started)
		<-ss.Context().Done()
		return nil
	}
	ended := make(chan error, 1)
	go func() { ended <- guard(nil, openStream{ctx: ctx}, nil, handler) }()
	<-started

	// While the token is good, the stream stays open.
	select {
	case err := <-ended:
		require.Fail(t, "the stream ended early", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The token is rotated: the stream ends, and the caller is told why.
	check.allow.Store(false)
	select {
	case err := <-ended:
		assert.Equal(t, codes.Unauthenticated, status.Code(err))
	case <-time.After(2 * time.Second):
		require.Fail(t, "the stream outlived its token")
	}
}

func TestAStreamWithoutAValidTokenNeverStarts(t *testing.T) {
	guard := guardStream(func(ctx context.Context) error { return allowedGRPC(ctx, &flip{}, Node) }, recheckEvery)
	called := false
	handler := func(any, grpc.ServerStream) error { called = true; return nil }

	err := guard(nil, openStream{ctx: context.Background()}, nil, handler)

	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.False(t, called)
}

// A watch and a log that is being followed are answers that go on. One
// started with the admin token must not outlive that token: after
// "token rotate admin" nobody but the holder of the new one may be told
// anything more.
func TestAnAnswerThatGoesOnIsCutOffWhenItsTokenStopsBeingValid(t *testing.T) {
	check := &flip{}
	check.allow.Store(true)
	// The handler is a watch: it says something, and then waits for more to say.
	watch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintln(w, "event: Synced")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})
	server := httptest.NewServer(requireHTTP(check, Admin, watch, 10*time.Millisecond))
	defer server.Close()
	resp, err := http.Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewReader(resp.Body)
	line, err := lines.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "event: Synced\n", line)
	ended := make(chan error, 1)
	go func() {
		_, err := lines.ReadString('\n')
		ended <- err
	}()

	// While the token is good, the answer goes on.
	select {
	case err := <-ended:
		require.Fail(t, "the answer ended early", "%v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// The token is rotated: the connection is cut, which the client can
	// tell from an answer that has come to its end.
	check.allow.Store(false)
	select {
	case err := <-ended:
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	case <-time.After(2 * time.Second):
		require.Fail(t, "the answer outlived its token")
	}
}

// An answer that ends by itself is not disturbed by the watching.
func TestAnAnswerThatEndsIsLeftWhole(t *testing.T) {
	check := &flip{}
	check.allow.Store(true)
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond) // long enough to be looked at again
		_, _ = fmt.Fprint(w, "done")
	})
	server := httptest.NewServer(requireHTTP(check, Admin, slow, time.Millisecond))
	defer server.Close()

	resp, err := http.Get(server.URL)

	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "done", string(body))
}

// FuzzToken: a token arrives in a header, from anyone who can reach the
// port. Whatever it is, it lets its bearer in only if it ends with the
// secret of the role or the admin's, after nothing or after one part
// without a "." in it: the fingerprint a join token starts with. And a
// secret that is empty lets nobody in.
func FuzzToken(f *testing.F) {
	s, err := Load(f.TempDir(), nil)
	require.NoError(f, err)
	empty := &Secrets{tokens: map[Role]string{Admin: "", Allocator: "", Node: ""}}
	for _, role := range roles {
		f.Add(s.Token(role), string(role))
		f.Add("fingerprint."+s.tokens[role], string(role))
		f.Add("a.b."+s.tokens[role], string(role))
		f.Add(s.tokens[role]+".", string(role))
	}
	f.Add("", "admin")
	f.Add(".", "join")
	f.Fuzz(func(t *testing.T, token, name string) {
		role := Role(name)
		assert.False(t, empty.Allows(role, token), "an empty secret must match nothing")

		prefix, secret := "", token
		if before, after, found := strings.Cut(token, "."); found {
			prefix, secret = before, after
		}
		good := secret != "" && (secret == s.tokens[role] || secret == s.tokens[Admin])
		assert.Equal(t, good, s.Allows(role, token), "token %q with the fingerprint %q for %q", token, prefix, role)
	})
}
