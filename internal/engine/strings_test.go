package engine

import (
	"bytes"
	"testing"
)

func TestUTF16LenMatrix(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want int
	}{
		{"ascii", []byte("hello"), 5},
		{"empty", []byte(""), 0},
		{"bmp", []byte("héllo"), 5}, // é = U+00E9 (2 WTF-8 bytes, 1 unit)
		{"astral", []byte("𝕏"), 2},  // U+1D54F (4 bytes, 2 units)
		{"mixed", []byte("a𝕏b"), 4}, // 1 + 2 + 1
		{"cjk", []byte("日本語"), 3},   // each 3 bytes, 1 unit
	}
	for _, c := range cases {
		if got := utf16Len(c.b); got != c.want {
			t.Errorf("%s: utf16Len=%d want %d", c.name, got, c.want)
		}
	}
}

func TestUTF16CodeUnitAt(t *testing.T) {
	// "a𝕏b": units are 'a', highSurrogate, lowSurrogate, 'b'.
	b := []byte("a𝕏b")
	want := []uint32{'a', 0xD835, 0xDD4F, 'b'}
	for i, w := range want {
		if got := utf16CodeUnitAt(b, i); got != w {
			t.Errorf("codeUnitAt(%d)=%#x want %#x", i, got, w)
		}
	}
	if utf16CodeUnitAt(b, 4) != 0xFFFFFFFF {
		t.Error("out-of-range should be 0xFFFFFFFF")
	}
	// BMP char
	if got := utf16CodeUnitAt([]byte("héllo"), 1); got != 0xE9 {
		t.Errorf("é codeUnit=%#x want 0xE9", got)
	}
}

func TestUTF16CodepointAt(t *testing.T) {
	b := []byte("a𝕏b")
	if got := utf16CodepointAt(b, 1); got != 0x1D54F {
		t.Errorf("codepointAt(1)=%#x want 0x1D54F", got)
	}
	// At the low surrogate index, returns the low surrogate.
	if got := utf16CodepointAt(b, 2); got != 0xDD4F {
		t.Errorf("codepointAt(2)=%#x want 0xDD4F", got)
	}
	if got := utf16CodepointAt(b, 0); got != 'a' {
		t.Errorf("codepointAt(0)=%#x want 'a'", got)
	}
}

func TestLoneSurrogate(t *testing.T) {
	b := wtf8Encode(nil, 0xD800)
	if len(b) != 3 {
		t.Fatalf("lone surrogate WTF-8 len=%d want 3", len(b))
	}
	if utf16Len(b) != 1 {
		t.Errorf("lone surrogate utf16Len=%d want 1", utf16Len(b))
	}
	if got := utf16CodeUnitAt(b, 0); got != 0xD800 {
		t.Errorf("lone surrogate codeUnit=%#x want 0xD800", got)
	}
}

func TestIndexToByteOffset(t *testing.T) {
	b := []byte("a𝕏b") // bytes: a=1, astral=4, b=1 => 6 bytes; units 0..3
	// unit 0 -> byte 0 (1 byte), unit 1 -> byte 1 (4 bytes, the astral char),
	// unit 3 -> byte 5 ('b').
	off, cb, ok := utf16IndexToByteOffset(b, 0)
	if !ok || off != 0 || cb != 1 {
		t.Errorf("idx0 -> off=%d cb=%d ok=%v", off, cb, ok)
	}
	off, cb, ok = utf16IndexToByteOffset(b, 1)
	if !ok || off != 1 || cb != 4 {
		t.Errorf("idx1 -> off=%d cb=%d ok=%v", off, cb, ok)
	}
	off, cb, ok = utf16IndexToByteOffset(b, 3)
	if !ok || off != 5 || cb != 1 {
		t.Errorf("idx3 -> off=%d cb=%d ok=%v", off, cb, ok)
	}
	// End index maps to end offset.
	off, _, ok = utf16IndexToByteOffset(b, 4)
	if !ok || off != len(b) {
		t.Errorf("idx4 (end) -> off=%d ok=%v", off, ok)
	}
	if _, _, ok := utf16IndexToByteOffset(b, 5); ok {
		t.Error("past-end index should not be ok")
	}
}

func TestByteOffsetToUtf16RoundTrip(t *testing.T) {
	// Round-trip is exact for *character-boundary* byte offsets. (A UTF-16 index
	// pointing mid-surrogate-pair, e.g. at a low surrogate, has no byte boundary
	// of its own; utf16IndexToByteOffset overshoots to the next char, matching
	// ant. So we iterate char boundaries, not raw UTF-16 indices.)
	b := []byte("a𝕏b日")
	// character boundaries: a[0,1) 𝕏[1,5) b[5,6) 日[6,9)
	boundaries := []int{0, 1, 5, 6, 9}
	for _, off := range boundaries {
		idx := byteOffsetToUtf16(b, off)
		back, _, ok := utf16IndexToByteOffset(b, idx)
		if !ok || back != off {
			t.Errorf("boundary off=%d -> idx=%d -> off=%d ok=%v", off, idx, back, ok)
		}
	}
}

func TestRangeToByteRange(t *testing.T) {
	b := []byte("a𝕏b") // units: a[0] astral[1,2] b[3]
	// substring(0,1) -> "a" bytes [0,1)
	bs, be := utf16RangeToByteRange(b, 0, 1)
	if !bytes.Equal(b[bs:be], []byte("a")) {
		t.Errorf("range(0,1)=%q want a", b[bs:be])
	}
	// substring(1,3) -> the astral char (both surrogate units) -> bytes [1,5)
	bs, be = utf16RangeToByteRange(b, 1, 3)
	if !bytes.Equal(b[bs:be], []byte("𝕏")) {
		t.Errorf("range(1,3)=%q want astral", b[bs:be])
	}
	// substring(3,4) -> "b"
	bs, be = utf16RangeToByteRange(b, 3, 4)
	if !bytes.Equal(b[bs:be], []byte("b")) {
		t.Errorf("range(3,4)=%q want b", b[bs:be])
	}
}

func TestUTF16ToWTF8SurrogatePair(t *testing.T) {
	// High+low surrogate should combine into the astral code point.
	units := []uint16{0xD835, 0xDD4F}
	b := utf16ToWTF8(units)
	if !bytes.Equal(b, []byte("𝕏")) {
		t.Errorf("combined pair = %q want 𝕏", b)
	}
	// A lone high surrogate stays as WTF-8.
	b2 := utf16ToWTF8([]uint16{0xD835})
	if utf16Len(b2) != 1 || utf16CodeUnitAt(b2, 0) != 0xD835 {
		t.Errorf("lone high surrogate mishandled: %x", b2)
	}
}

func TestStringPoolAndIntern(t *testing.T) {
	rt := New()
	s := rt.newString("hello")
	if !s.IsString() {
		t.Fatal("not a string")
	}
	if !bytes.Equal(rt.strBytes(s), []byte("hello")) {
		t.Errorf("bytes=%q", rt.strBytes(s))
	}
	if !rt.strIsASCII(s) {
		t.Error("hello should be ASCII")
	}
	if rt.strIsASCII(rt.newString("héllo")) {
		t.Error("héllo should not be ASCII")
	}
	// Interning returns the same underlying handle.
	a := rt.internString("dup")
	b := rt.internString("dup")
	if strHandle(a) != strHandle(b) {
		t.Error("intern should return the same handle")
	}
}
