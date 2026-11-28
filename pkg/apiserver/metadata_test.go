package apiserver

import (
	"encoding/json"
	"net/http"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const simplePath = gameServersPath + "/simple"

func createSimple(t *testing.T, h http.Handler) *agonesv1.GameServer {
	t.Helper()
	body := replace(t, manifest, "  name: simple\n", `  name: simple
  labels: {mode: casual, agones.dev/sdk-map: dust}
  annotations: {note: first}
`)
	rec := call(t, h, http.MethodPost, gameServersPath, body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	gs := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), gs))
	return gs
}

// Like "kubectl label" and "kubectl annotate": the keys named are set or
// removed, and the others stay, the one the game server set through the
// SDK among them. Nothing else of the GameServer changes.
func TestALabelOrAnnotationOfAGameServerCanBeChanged(t *testing.T) {
	h := newServer(t)
	before := createSimple(t, h)

	rec := call(t, h, http.MethodPatch, simplePath,
		`{"metadata": {"labels": {"mode": "ranked", "region": "east"}, "annotations": {"note": null}}}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := &agonesv1.GameServer{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), after))
	assert.Equal(t, map[string]string{"mode": "ranked", "region": "east", "agones.dev/sdk-map": "dust"}, after.Labels)
	assert.NotContains(t, after.Annotations, "note")
	assert.Equal(t, before.Spec, after.Spec)
	assert.Equal(t, before.Status, after.Status)
}

func TestOnlyTheLabelsAndAnnotationsOfAGameServerCanBeChanged(t *testing.T) {
	tests := map[string]string{
		"its spec":                        `{"spec": {"container": "other"}}`,
		"its status":                      `{"status": {"state": "Ready"}}`,
		"its name":                        `{"metadata": {"name": "other"}}`,
		"an annotation that scrim writes": `{"metadata": {"annotations": {"scrim/last-request": "made-up"}}}`,
		"a label value that is not one":   `{"metadata": {"labels": {"mode": "not a label value!"}}}`,
		"a label key that is not one":     `{"metadata": {"labels": {"not/a/key": "x"}}}`,
	}
	for name, patch := range tests {
		t.Run(name, func(t *testing.T) {
			h := newServer(t)
			before := createSimple(t, h)

			rec := call(t, h, http.MethodPatch, simplePath, patch)

			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			after := call(t, h, http.MethodGet, simplePath, "")
			assert.JSONEq(t, mustJSON(t, before), after.Body.String(), "nothing changed")
		})
	}
}

func TestAGameServerThatIsNotThereCannotBeChanged(t *testing.T) {
	h := newServer(t)

	rec := call(t, h, http.MethodPatch, simplePath, `{"metadata": {"labels": {"mode": "ranked"}}}`)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return string(out)
}
