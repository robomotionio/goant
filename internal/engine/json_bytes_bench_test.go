package engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

// benchMessage builds a message shaped like the ones a FunctionNode carries:
// an array of small records, which is what dominates real payloads.
func benchMessage(n int) []byte {
	rows := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		rows[i] = map[string]any{
			"id":     fmt.Sprintf("row-%d", i),
			"name":   fmt.Sprintf("value %d", i),
			"amount": i * 7,
		}
	}
	b, _ := json.Marshal(map[string]any{"records": rows})
	return b
}

// The two inbound paths.
//
// viaString is what a host without byte entry points must do: copy the bytes
// into a JS string, then have the engine parse that string. viaBytes parses the
// host's bytes directly.
func BenchmarkJSONInboundViaString(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		msg := benchMessage(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			rt := New()
			b.ReportAllocs()
			b.SetBytes(int64(len(msg)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// The copy a host pays to hand bytes over as a string.
				s := rt.NewStringData(string(msg))
				if _, err := rt.JSONParse(rt.strGo(s)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkJSONInboundViaBytes(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		msg := benchMessage(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			rt := New()
			b.ReportAllocs()
			b.SetBytes(int64(len(msg)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := rt.JSONParseBytes(msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// The two outbound paths.
//
// viaDoubleStringify reproduces what the FunctionNode wrapper does: stringify
// each output, collect the strings in an array, stringify that array so it can
// travel back as one value, then take it apart again on the host side. Every
// quote in every payload is escaped twice and the whole thing is copied an
// extra time.
//
// viaBytes writes each output straight into one buffer, with offsets.
func BenchmarkJSONOutboundViaDoubleStringify(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		msg := benchMessage(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			rt := New()
			v, err := rt.JSONParseBytes(msg)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(msg)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Inner: serialize the payload.
				inner, ok, err := rt.JSONStringify(v)
				if err != nil || !ok {
					b.Fatalf("inner: ok=%v err=%v", ok, err)
				}
				// Outer: pack it into an array of strings and serialize again.
				arr := rt.newArray()
				rt.arraySet(rt.objPtr(arr), 0, rt.NewStringData(inner))
				outer, _, err := rt.JSONStringify(arr)
				if err != nil {
					b.Fatal(err)
				}
				// Host side: take it apart again.
				var results []string
				if err := json.Unmarshal([]byte(outer), &results); err != nil {
					b.Fatal(err)
				}
				if len(results) != 1 || len(results[0]) == 0 {
					b.Fatal("lost the payload")
				}
				_ = []byte(results[0])
			}
		})
	}
}

func BenchmarkJSONOutboundViaBytes(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		msg := benchMessage(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			rt := New()
			v, err := rt.JSONParseBytes(msg)
			if err != nil {
				b.Fatal(err)
			}
			buf := make([]byte, 0, len(msg)+1024)
			b.ReportAllocs()
			b.SetBytes(int64(len(msg)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, _, err := rt.JSONStringifyEachToBytes([]Value{v}, buf[:0])
				if err != nil {
					b.Fatal(err)
				}
				if len(out) == 0 {
					b.Fatal("lost the payload")
				}
			}
		})
	}
}
