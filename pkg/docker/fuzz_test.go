package docker

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fuzz tests of this package feed its parsers what they get from
// outside: image references from manifests, the configuration file of the
// docker command, and what the daemon sends. None may panic, and each has
// something that must hold whatever it is given. Run one with
//
//	go test -fuzz=FuzzImageReference -fuzztime=2m ./pkg/docker

var sha = "sha256:" + strings.Repeat("ab", 32)

// FuzzImageReference: an image reference is taken apart in two ways, into
// name and tag for the pull and into its registry for the credentials.
//
// For any text at all, a pull names one tag or one digest: an empty tag
// would ask the daemon for every tag there is. For a reference the daemon
// can parse, the parts are references themselves and put together give the
// same reference again, and the registry does not hang on the tag.
func FuzzImageReference(f *testing.F) {
	for _, image := range []string{
		"alpine", "alpine:3.20", "alpine:", "library/alpine", "localhost:5000/game", "localhost:5000/game:v1",
		"[::1]:5000/game:v1", "Registry/game", "index.docker.io/library/alpine", "a__b/c-d.e", "localhost",
		"example.com/game@" + sha, "example.com/game:v1@" + sha, "", ":", "@", "a:b:c", "a/b:c/d", "alpine@sha256:abc",
	} {
		f.Add(image)
	}
	f.Fuzz(func(t *testing.T, image string) {
		name, tag := SplitTag(image)
		registry := registryOf(image)

		if strings.Contains(image, "@") {
			assert.Equal(t, []string{image, ""}, []string{name, tag}, "a digest names the image by itself")
		} else {
			assert.NotEmpty(t, tag, "a pull without a digest names one tag")
		}
		if !ValidImage(image) || strings.Contains(image, "@") {
			return
		}
		assert.True(t, ValidImage(name), "the name of %q is %q", image, name)
		assert.True(t, ValidImage(name+":"+tag), "the name and tag of %q are %q and %q", image, name, tag)
		assert.Contains(t, []string{name, name + ":" + tag}, image, "name and tag make the reference again")
		again, same := SplitTag(name + ":" + tag)
		assert.Equal(t, []string{name, tag}, []string{again, same}, "taking it apart twice changes nothing")

		assert.Equal(t, registry, registryOf(name), "the registry of %q does not hang on its tag", image)
		if registry != hub {
			assert.True(t, strings.HasPrefix(image, registry+"/"), "the registry %q is how %q starts", registry, image)
		}
	})
}

// FuzzCredentials: the configuration file of the docker command is read
// for the credentials of one registry.
//
// Whatever is in the file, what is found is either nothing or something to
// log in with, filed under a key that is this registry; and the header
// made of it is those credentials again when the daemon decodes it.
func FuzzCredentials(f *testing.F) {
	f.Add([]byte(dockerConfig), "docker.io")
	f.Add([]byte(dockerConfig), "registry.example.com:5000")
	f.Add([]byte(dockerConfig), "azure.example.com")
	f.Add([]byte(dockerConfig), "broken.example.com")
	f.Add([]byte(`{"auths": {"": {"auth": "Og=="}, "http://": {"username": "u"}}}`), "")
	f.Add([]byte(`{"auths": null}`), "ghcr.io")
	f.Add([]byte(`[]`), "ghcr.io")
	f.Fuzz(func(t *testing.T, config []byte, registry string) {
		found, err := credentialsFor(config, registry)
		if err != nil || found == (credentials{}) {
			assert.Equal(t, credentials{}, found, "an error comes with no credentials")
			return
		}
		assert.Equal(t, registry, hostOf(found.ServerAddress), "found under a key for another registry")
		assert.NotEmpty(t, found.Username+found.IdentityToken, "found, but nothing to log in with")
		assert.Empty(t, found.Auth, `"auth" is for the file; the daemon wants the user and the password`)

		raw, err := base64.URLEncoding.DecodeString(found.header())
		require.NoError(t, err)
		var sent credentials
		require.NoError(t, json.Unmarshal(raw, &sent))
		assert.Equal(t, asJSONKeepsIt(found), sent, "the header is the credentials")
	})
}

// asJSONKeepsIt is c with every byte that is not UTF-8 replaced by the
// replacement character, which is what writing it as JSON does. (A password
// with such bytes cannot be sent to the daemon at all: the header is JSON.)
func asJSONKeepsIt(c credentials) credentials {
	replaced := func(text string) string { return string([]rune(text)) }
	c.Username, c.Password = replaced(c.Username), replaced(c.Password)
	c.IdentityToken, c.ServerAddress = replaced(c.IdentityToken), replaced(c.ServerAddress)
	return c
}

// FuzzLogin: what "docker login" writes for a user and a password is read
// back as that user and that password, under every form of key it has used.
func FuzzLogin(f *testing.F) {
	f.Add("ghcr.io", "octo", "ghp_token")
	f.Add("registry.example.com:5000", "user", "pass:with:colons")
	f.Add("localhost", "u", "")
	f.Fuzz(func(t *testing.T, host, user, password string) {
		text := utf8.ValidString(host) && utf8.ValidString(user) && utf8.ValidString(password)
		if !text || user == "" || strings.Contains(user, ":") || strings.Contains(host, "/") {
			t.Skip("not something docker login would write")
		}
		for _, key := range []string{host, "https://" + host, "https://" + host + "/v1/", "http://" + host + "/v2/"} {
			auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
			config, err := json.Marshal(map[string]any{"auths": map[string]any{key: map[string]string{"auth": auth}}})
			require.NoError(t, err)

			found, err := credentialsFor(config, hostOf(key))

			require.NoError(t, err)
			assert.Equal(t, credentials{Username: user, Password: password, ServerAddress: key}, found)
		}
	})
}

// FuzzPullProgress: the daemon answers a pull with a stream of progress
// messages, and a failure is one of them.
//
// Whatever the stream holds, reading it ends. And a failure that follows a
// stream that was fine so far is reported, in the daemon's words: or a
// pull that failed would count as done, and the container would be
// started from an image that is not there.
func FuzzPullProgress(f *testing.F) {
	f.Add([]byte(`{"status":"Pulling"}`+"\n"+`{"status":"Done"}`), "manifest unknown")
	f.Add([]byte(`{"error":"manifest unknown"}`), "x")
	f.Add([]byte(`{"errorDetail":{"message":""},"error":""} null 1`), "denied")
	f.Add([]byte(`{"status":`), "denied")
	f.Add([]byte(``), "")
	f.Fuzz(func(t *testing.T, progress []byte, failure string) {
		if pullError(bytes.NewReader(progress)) != nil || failure == "" || !utf8.ValidString(failure) {
			return
		}
		for _, field := range []string{"error", "errorDetail"} {
			message := map[string]any{"error": failure}
			if field == "errorDetail" {
				message = map[string]any{"errorDetail": map[string]string{"message": failure}}
			}
			last, err := json.Marshal(message)
			require.NoError(t, err)

			err = pullError(io.MultiReader(bytes.NewReader(progress), bytes.NewReader(last)))

			assert.EqualError(t, err, failure, "after %q", progress)
		}
	})
}

// FuzzLogFrames: Docker sends the output of a container in frames, and the
// connection delivers them in pieces of any size.
//
// However the bytes arrive, and however many are asked for at a time, the
// reader gives the same output: what the whole frames hold, in order. It
// ends without an error exactly when the stream ends between two frames.
func FuzzLogFrames(f *testing.F) {
	f.Add([]byte("ready\nwarning: low memory\nbye\n"), []byte{6, 20, 0, 4}, []byte{}, []byte{1, 2, 3}, uint8(3))
	f.Add([]byte("cut short"), []byte{9}, []byte{1, 0, 0, 0, 0, 0, 0, 5, 'a', 'b'}, []byte{8}, uint8(1))
	f.Add([]byte{}, []byte{0, 0, 0}, []byte{2, 0, 0}, []byte{1}, uint8(0))
	f.Fuzz(func(t *testing.T, output, lengths, tail, pieces []byte, ask uint8) {
		wire := append(framed(output, lengths), tail...)
		want, whole := unframed(wire)

		got, err := readInPieces(&frames{body: io.NopCloser(&inPieces{data: wire, sizes: pieces})}, int(ask)+1)

		assert.Equal(t, want, got)
		if whole {
			assert.NoError(t, err)
		} else {
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF, "the stream ends inside a frame")
		}
	})
}

// framed wraps output the way Docker does, in frames whose lengths are
// taken from lengths for as long as there is output left.
func framed(output, lengths []byte) []byte {
	var wire []byte
	for _, length := range lengths {
		n := min(int(length), len(output))
		wire = append(wire, 1+length%2, 0, 0, 0)
		wire = binary.BigEndian.AppendUint32(wire, uint32(n))
		wire = append(wire, output[:n]...)
		output = output[n:]
	}
	return wire
}

// unframed is the slow, obvious reading of a stream that is all there: the
// output in its frames, and whether it ends where a frame ends.
func unframed(wire []byte) (output []byte, whole bool) {
	output = []byte{}
	for len(wire) >= 8 {
		length := int(binary.BigEndian.Uint32(wire[4:8]))
		wire = wire[8:]
		if length > len(wire) {
			return append(output, wire...), false
		}
		output = append(output, wire[:length]...)
		wire = wire[length:]
	}
	return output, len(wire) == 0
}

// inPieces delivers data a piece at a time, the sizes of the pieces taken
// in turn from sizes: the way a connection delivers a stream.
type inPieces struct {
	data  []byte
	sizes []byte
	next  int
}

func (r *inPieces) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	size := 1
	if len(r.sizes) > 0 {
		size = int(r.sizes[r.next%len(r.sizes)])%16 + 1
		r.next++
	}
	n := copy(p, r.data[:min(size, len(r.data))])
	r.data = r.data[n:]
	return n, nil
}

// readInPieces reads r to its end, asking for at most ask bytes at a time.
// A reader that keeps answering with nothing is stopped: it would keep its
// caller busy for ever.
func readInPieces(r io.Reader, ask int) ([]byte, error) {
	out, buffer, idle := []byte{}, make([]byte, ask), 0
	for idle < 100 {
		n, err := r.Read(buffer)
		out = append(out, buffer[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		idle++
		if n > 0 {
			idle = 0
		}
	}
	return out, io.ErrNoProgress
}

// FuzzAPIVersions: the daemon says which versions of its API it speaks, as
// two pieces of text. The version agreed on is one both sides know.
func FuzzAPIVersions(f *testing.F) {
	f.Add("1.51", "1.24")
	f.Add("1.40", "1.12")
	f.Add("1.99", "1.60")
	f.Add("1.45", "1.50")
	f.Add("", "x")
	f.Add("1.44abc", "-1.5")
	f.Fuzz(func(t *testing.T, newest, oldest string) {
		agreed, err := negotiate(newest, oldest)
		if err != nil {
			return
		}
		version, err := parseAPIVersion(agreed)
		require.NoError(t, err)
		daemonNewest, _ := parseAPIVersion(newest)
		daemonOldest, _ := parseAPIVersion(oldest)
		assert.False(t, version.before(oldestAPI) || newestAPI.before(version), "%s is not a version this client speaks", agreed)
		assert.False(t, version.before(daemonOldest) || daemonNewest.before(version), "%s is not a version the daemon speaks: it said %q to %q", agreed, oldest, newest)
	})
}
