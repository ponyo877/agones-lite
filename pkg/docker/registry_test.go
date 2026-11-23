package docker

import (
	"context"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryOf(t *testing.T) {
	tests := map[string]string{
		"alpine":                           "docker.io",
		"alpine:3.20":                      "docker.io",
		"example/game:1":                   "docker.io",
		"docker.io/library/alpine":         "docker.io",
		"index.docker.io/library/alpine":   "docker.io",
		"localhost/game":                   "localhost",
		"localhost:5000/game:v1":           "localhost:5000",
		"ghcr.io/example/game":             "ghcr.io",
		"us-docker.pkg.dev/a/b/game:0.44":  "us-docker.pkg.dev",
		"registry.example.com/game@sha256": "registry.example.com",
		"[::1]:5000/game":                  "[::1]:5000",
		"Registry/game":                    "Registry",
		"localhost":                        "docker.io", // no "/": a repository that happens to be called so
	}
	for image, want := range tests {
		assert.Equal(t, want, registryOf(image), image)
	}
}

// What "docker login" leaves behind, in the forms it has had over the years.
const dockerConfig = `{
	"auths": {
		"https://index.docker.io/v1/": {"auth": "aHViLXVzZXI6aHViLXBhc3M="},
		"ghcr.io": {"username": "octo", "password": "ghp_token"},
		"https://registry.example.com:5000/v2/": {"auth": "dXNlcjpwYXNzOndpdGg6Y29sb25z"},
		"azure.example.com": {"auth": "MDAwMDAwMDAtMDAwMDo=", "identitytoken": "refresh-token"},
		"helper.example.com": {},
		"broken.example.com": {"auth": "bm8tY29sb24="}
	},
	"credsStore": "desktop",
	"credHelpers": {"gcr.io": "gcloud"}
}`

func TestCredentialsFor(t *testing.T) {
	tests := map[string]credentials{
		"docker.io":                 {Username: "hub-user", Password: "hub-pass", ServerAddress: "https://index.docker.io/v1/"},
		"ghcr.io":                   {Username: "octo", Password: "ghp_token", ServerAddress: "ghcr.io"},
		"registry.example.com:5000": {Username: "user", Password: "pass:with:colons", ServerAddress: "https://registry.example.com:5000/v2/"},
		"azure.example.com":         {Username: "00000000-0000", IdentityToken: "refresh-token", ServerAddress: "azure.example.com"},
		"helper.example.com":        {}, // the secret is with a credential helper, which we do not run
		"gcr.io":                    {},
		"registry.example.com":      {}, // another port is another registry
		"unknown.example.com":       {},
	}
	for registry, want := range tests {
		found, err := credentialsFor([]byte(dockerConfig), registry)
		require.NoError(t, err, registry)
		assert.Equal(t, want, found, registry)
	}
}

func TestCredentialsThatCannotBeReadAreAnError(t *testing.T) {
	_, err := credentialsFor([]byte(dockerConfig), "broken.example.com")
	assert.ErrorContains(t, err, `auths["broken.example.com"]`)

	_, err = credentialsFor([]byte(`{"auths": {"ghcr.io": {"auth": "not base64!"}}}`), "ghcr.io")
	assert.ErrorContains(t, err, `auths["ghcr.io"]`)

	_, err = credentialsFor([]byte(`{"auths": `), "ghcr.io")
	assert.Error(t, err)

	// What is wrong with another registry's entry does not matter.
	_, err = credentialsFor([]byte(dockerConfig), "ghcr.io")
	assert.NoError(t, err)
}

func TestTheHeaderIsTheCredentialsAsURLSafeBase64(t *testing.T) {
	header := credentials{Username: "octo", Password: "p?ss>", ServerAddress: "ghcr.io"}.header()

	raw, err := base64.URLEncoding.DecodeString(header)
	require.NoError(t, err)
	assert.JSONEq(t, `{"username": "octo", "password": "p?ss>", "serveraddress": "ghcr.io"}`, string(raw))

	token := credentials{IdentityToken: "refresh-token"}.header()
	raw, err = base64.URLEncoding.DecodeString(token)
	require.NoError(t, err)
	assert.JSONEq(t, `{"identitytoken": "refresh-token"}`, string(raw))

	assert.Empty(t, credentials{}.header(), "nothing to log in with, so no header")
	assert.Empty(t, credentials{Password: "alone"}.header())
}

func TestPullSendsTheCredentialsOfTheImagesRegistry(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(config, []byte(dockerConfig), 0o600))
	tests := map[string]struct {
		config string
		image  string
		want   string // the credentials sent, as JSON; empty for none
	}{
		"a private registry": {
			config, "ghcr.io/example/game:1",
			`{"username": "octo", "password": "ghp_token", "serveraddress": "ghcr.io"}`,
		},
		"Docker Hub": {
			config, "alpine:3.20",
			`{"username": "hub-user", "password": "hub-pass", "serveraddress": "https://index.docker.io/v1/"}`,
		},
		"a registry the file does not know":      {config, "quay.io/example/game:1", ""},
		"a registry left to a credential helper": {config, "gcr.io/example/game:1", ""},
		"no file given":                          {"", "ghcr.io/example/game:1", ""},
		"a file that is not there":               {filepath.Join(t.TempDir(), "none.json"), "ghcr.io/example/game:1", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var sent []string
			c := fakeDaemon(t, func(_ http.ResponseWriter, r *http.Request) {
				sent = r.Header.Values("X-Registry-Auth")
			})
			c.ConfigFile = tc.config

			require.NoError(t, c.Pull(context.Background(), tc.image))

			if tc.want == "" {
				assert.Empty(t, sent)
				return
			}
			require.Len(t, sent, 1)
			raw, err := base64.URLEncoding.DecodeString(sent[0])
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(raw))
		})
	}
}

func TestPullFailsOnAConfigFileItCannotRead(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(config, []byte(`{"auths": `), 0o600))
	asked := false
	c := fakeDaemon(t, func(http.ResponseWriter, *http.Request) { asked = true })
	c.ConfigFile = config

	err := c.Pull(context.Background(), "ghcr.io/example/game:1")

	assert.ErrorContains(t, err, config)
	assert.False(t, asked, "better no pull than one that fails for a reason nobody sees")
}
