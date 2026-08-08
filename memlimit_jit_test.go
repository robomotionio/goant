package goant

import (
	"strings"
	"testing"
	"time"
)

// The memory limit has to bound a script whatever tier it is running on.
//
// It is the property this engine is chosen for: a script that asks for more
// than it is allowed becomes an error the host can catch and route, instead of
// an out-of-memory that takes the process down. A compiled loop is still a
// loop, and for a while it was not bounded at all — the limit is judged on what
// survived the last sweep, and a compiled loop triggered no sweeps, so it
// measured every allocation against a figure from before it started.
//
// The var-headed case is the one that regressed, because a let-headed loop was
// not compiled at all at the time. Both are here now: which loop forms the tier
// accepts is exactly the kind of thing that changes underneath a test like
// this, and a test that only covers the forms it happened to accept on the day
// it was written stops covering anything.
func TestTheMemoryLimitBoundsACompiledLoop(t *testing.T) {
	loops := []struct{ name, src string }{
		{"let header", `const held = []; for (let i = 0; i < 20000000; i++) held.push({ i: i, s: "padding " + i }); held.length`},
		{"var header", `var held = []; for (var i = 0; i < 20000000; i++) held.push({ i: i, s: "padding " + i }); held.length`},
		{"inside a function", `function f() { var h = []; for (var i = 0; i < 20000000; i++) h.push({ i: i, s: "padding " + i }); return h.length; } f()`},
		{"while loop", `var held = []; var i = 0; while (i < 20000000) { held.push({ i: i, s: "padding " + i }); i++; } held.length`},
	}
	for _, c := range loops {
		for _, jit := range []bool{false, true} {
			name := c.name + "/interpreter"
			if jit {
				name = c.name + "/tier on"
			}
			t.Run(name, func(t *testing.T) {
				rt := New(WithMemoryLimit(16<<20), WithJIT(jit))
				defer rt.Close()

				// On a goroutine with a deadline, because the failure this
				// guards against is not a wrong answer but no answer: the loop
				// runs until something else gives up, and the host reports
				// whatever that was. A t.Fatal beats a hung test.
				done := make(chan error, 1)
				go func() { _, err := rt.RunString(c.src); done <- err }()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("a script that blew the budget returned successfully")
					}
					if !strings.Contains(strings.ToLower(err.Error()), "memory") {
						t.Fatalf("stopped, but not for memory: %v", err)
					}
				case <-time.After(30 * time.Second):
					t.Fatal("the budget did not bound the loop")
				}
			})
		}
	}
}
