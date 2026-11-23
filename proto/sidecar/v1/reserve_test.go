package sidecarv1

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAReserveLastsAsLongAsADurationCanSay(t *testing.T) {
	const longest = time.Duration(math.MaxInt64/int64(time.Second)) * time.Second // 292 years
	tests := map[string]struct {
		seconds int64
		want    time.Duration
	}{
		"a minute":                    {seconds: 60, want: time.Minute},
		"until told otherwise":        {seconds: 0, want: 0},
		"negative counts as zero":     {seconds: -5, want: 0},
		"the last second that fits":   {seconds: 9_223_372_036, want: longest},
		"one more wraps to minus":     {seconds: 9_223_372_037, want: longest},
		"enough more wraps to 0.3s":   {seconds: 18_446_744_074, want: longest},
		"far enough below wraps too":  {seconds: -10_000_000_000, want: 0},
		"as many as an int64 can say": {seconds: math.MaxInt64, want: longest},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, ReserveFor(tc.seconds))
		})
	}
}
