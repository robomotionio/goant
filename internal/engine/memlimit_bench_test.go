package engine

import "testing"

func benchScript(b *testing.B, src string) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rt := New()
		sc, err := rt.CompileScript("b.js", src)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := rt.RunScript(sc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkChurnNoLimit(b *testing.B) {
	benchScript(b, `let n=0; for (let i=0;i<300000;i++){ const o={a:i,b:i+1}; n+=o.a+o.b; } n;`)
}

func BenchmarkArrayBuildNoLimit(b *testing.B) {
	benchScript(b, `let t=0; for(let r=0;r<3;r++){const a=[];for(let i=0;i<100000;i++)a.push(i*2);for(let i=0;i<a.length;i++)t+=a[i];} t;`)
}

func BenchmarkStringBuildNoLimit(b *testing.B) {
	benchScript(b, `let n=0; for(let r=0;r<20;r++){let s="";for(let i=0;i<5000;i++)s+="abcdefgh";n+=s.length;} n;`)
}
