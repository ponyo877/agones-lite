package apiserver

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ponyo877/scrim/pkg/agones"
	"github.com/ponyo877/scrim/pkg/store"
)

// FuzzAdmit hands admission anything at all as a manifest, in a namespace
// that is anything at all, for each kind a client can write.
//
// Admission may refuse it; most of what the fuzzer makes is refused. It
// must not panic. And what it lets through must be safe for everything
// that comes after, which is what the oracle in safe_test.go says in full.
//
// The fuzzer starts from manifests that are right (upstream's samples and
// the ones under testdata/manifests, which set every field that reaches
// Docker) and from the ones the other tests expect to be refused: one small
// change away from each of those is where a missing check would show.
//
//	go test -fuzz=FuzzAdmit -fuzztime=2m -fuzzminimizetime=0 ./pkg/apiserver
//
// Without -fuzzminimizetime=0 the fuzzer spends most of its time making
// each input that reaches new code smaller, a minute at a time, and all
// its workers wait meanwhile.
func FuzzAdmit(f *testing.F) {
	require.NoError(f, agones.Setup())
	for _, manifest := range seedManifests(f) {
		f.Add("default", []byte(manifest))
	}
	f.Add("a_b", []byte(manifest))
	f.Add("default", []byte(inContainer(f, "          resources: {limits: {cpu: 1m, memory: 6Mi}}\n")))
	api := New(nil, nil, nil, nil) // admission never asks the store

	f.Fuzz(func(t *testing.T, namespace string, manifest []byte) {
		if gs, ok := admitted(api.servers, namespace, manifest); ok {
			safe(t, api, gs)
		}
		if fleet, ok := admitted(api.fleets, namespace, manifest); ok {
			safe(t, api, fleet)
		}
		if scaler, ok := admitted(api.scalers, namespace, manifest); ok {
			safe(t, api, scaler)
		}
	})
}

// admitted sends a manifest through the admission of one kind, the way a
// request does, and reports whether it was let through.
func admitted[T store.Object](r resource[T], namespace string, manifest []byte) (T, bool) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(manifest))
	req.SetPathValue("namespace", namespace)
	obj, _, err := r.read(req)
	return obj, err == nil
}

// seedManifests collects every manifest the tests of this package know:
// the ones that must be admitted, and the ones that must not.
func seedManifests(t testing.TB) []string {
	seeds := []string{manifest, fleetManifest, scalerManifest, fleetWithTemplateMetadata(t), gameServerWithPodMetadata(t), developmentGameServer(t)}
	seeds = append(seeds, manifestFiles(t, "testdata/manifests/*.yaml")...)
	seeds = append(seeds, manifestFiles(t, "../../third_party/agones/examples/*.yaml")...)
	seeds = append(seeds, manifestFiles(t, "../../third_party/agones/examples/*/*.yaml")...)
	for _, tc := range refusedByTheSchema(t) {
		seeds = append(seeds, tc.body)
	}
	for _, tc := range refusedForTheirNames(t) {
		seeds = append(seeds, tc.body)
	}
	for _, tc := range staticPortCases() {
		seeds = append(seeds, withPorts(t, tc.ports))
	}
	for _, tc := range whatCannotRun() {
		seeds = append(seeds, tc.body)
	}
	for _, tc := range refusedByThePod(t) {
		seeds = append(seeds, tc.body)
	}
	return seeds
}

// manifestFiles reads the Agones resources in the files that match pattern,
// one seed for each YAML document. Without the Agones submodule there are
// fewer seeds, and that is all.
func manifestFiles(t testing.TB, pattern string) []string {
	files, err := filepath.Glob(pattern)
	require.NoError(t, err)
	var manifests []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, document := range strings.Split(string(raw), "\n---") {
			if strings.Contains(document, "agones.dev/v1") {
				manifests = append(manifests, document)
			}
		}
	}
	return manifests
}

// The manifests under testdata/manifests are there to put every field that
// matters in front of the fuzzer. They can only do that when admission
// takes them, whole and without a warning.
func TestTheSeedsThatSetEverythingAreAdmitted(t *testing.T) {
	seeds := map[string]string{
		"testdata/manifests/gameserver.yaml":      "/v1/namespaces/default/gameservers",
		"testdata/manifests/fleet.yaml":           "/v1/namespaces/default/fleets/everything",
		"testdata/manifests/fleetautoscaler.yaml": "/v1/namespaces/default/fleetautoscalers/everything",
	}
	for file, path := range seeds {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(file)
			require.NoError(t, err)
			method := http.MethodPut
			if strings.HasSuffix(path, "gameservers") {
				method = http.MethodPost
			}

			rec := call(t, newServer(t), method, path, string(body))

			assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.Empty(t, rec.Header().Values("Warning"))
		})
	}
}
