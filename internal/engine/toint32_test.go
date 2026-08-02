package engine

import (
	"math"
	"testing"
)

// TestToUint32AboveInt64Range pins the range that a truncation straight to
// int64 gets wrong.
//
// Go leaves that conversion undefined outside int64's range and amd64 answers
// INT64_MIN, which made every value at or above 2**63 report as zero. The
// expectations here are node's.
func TestToUint32AboveInt64Range(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want int32
	}{
		{1e20, 1661992960},
		{9223372036854777856, 2048}, // 2**63 + 2048, exactly representable
		{-9223372036854777856, -2048},
		{1e300, 0}, // a multiple of 2**32, so genuinely zero
		{math.Ldexp(1, 63), 0},
		{math.Ldexp(1, 64), 0},
		{2147483648, -2147483648},
		{4294967296, 0},
		{4294967297, 1},
		{-1.5, -1},
		{1.5, 1},
		{-2147483649, 2147483647},
		{0, 0},
		{math.Copysign(0, -1), 0},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
	} {
		if got := toInt32(tc.in); got != tc.want {
			t.Errorf("toInt32(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestToUint32AgreesWithToInt32 checks the two stay one conversion apart across
// the interesting boundaries.
func TestToUint32AgreesWithToInt32(t *testing.T) {
	for _, d := range []float64{
		0, 1, -1, 2147483647, 2147483648, 4294967295, 4294967296,
		1e20, -1e20, 9223372036854777856, 1e300, -1e300, 0.5, -0.5,
	} {
		if got, want := toInt32(d), int32(toUint32(d)); got != want {
			t.Errorf("toInt32(%v) = %d, but int32(toUint32) = %d", d, got, want)
		}
	}
}
