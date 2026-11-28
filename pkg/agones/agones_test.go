package agones

import (
	"os"
	"regexp"
	"testing"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionIsTheOneInGoMod(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	required := regexp.MustCompile(`(?m)^\s*agones\.dev/agones v(\S+)$`).FindSubmatch(gomod)
	require.NotNil(t, required, "go.mod requires no agones.dev/agones")
	assert.Equal(t, string(required[1]), Version)
}

func TestDefaultsSayWhichReleaseOfAgones(t *testing.T) {
	require.NoError(t, Setup())
	gs := &agonesv1.GameServer{}
	gs.ApplyDefaults()
	fleet := &agonesv1.Fleet{}
	fleet.ApplyDefaults()
	assert.Equal(t, Version, gs.Annotations[agonesv1.VersionAnnotation])
	assert.Equal(t, Version, fleet.Annotations[agonesv1.VersionAnnotation])
}
