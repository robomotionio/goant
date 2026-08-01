package goant_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robomotionio/goant"
)

func TestScopeIsolatesGlobals(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	s1, err := rt.NewScope()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.RunString(`globalThis.leaked = 1; var alsoLeaked = 2`); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := rt.NewScope()
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	v, err := s2.RunString(`typeof leaked + "/" + typeof alsoLeaked`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "undefined/undefined" {
		t.Fatalf("the previous scope's globals are still visible: %q", v.String())
	}
}

func TestScopeKeepsBuiltins(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	s, err := rt.NewScope()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	v, err := s.RunString(`JSON.stringify([1, 2].map(String))`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != `["1","2"]` {
		t.Fatalf("got %q", v.String())
	}
}

func TestScopeReportsPollution(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	clean, _ := rt.NewScope()
	clean.RunString(`const x = [1, 2, 3]`)
	if clean.Polluted() {
		t.Error("an ordinary script does not pollute")
	}
	clean.Close()
	if clean.Polluted() {
		t.Error("Polluted must agree with itself after Close")
	}

	dirty, _ := rt.NewScope()
	dirty.RunString(`Array.prototype.mine = function () { return 1 }`)
	if !dirty.Polluted() {
		t.Fatal("writing to a shared prototype must be reported")
	}
	dirty.Close()
	if !dirty.Polluted() {
		t.Fatal("Polluted must still report after Close")
	}
}

func TestScopeRunsACompiledProgramManyTimes(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p, err := rt.Compile("main.js", `JSON.stringify({doubled: input * 2})`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		s, err := rt.NewScope()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Set("input", i); err != nil {
			t.Fatal(err)
		}
		v, err := s.RunProgram(p)
		if err != nil {
			t.Fatal(err)
		}
		got := v.String()
		s.Close()

		want := `{"doubled":` + itoa(i*2) + `}`
		if got != want {
			t.Fatalf("run %d = %s, want %s", i, got, want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestScopeReclaimsMemory(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	p, err := rt.Compile("alloc.js", `
		const rows = [];
		for (let i = 0; i < 2000; i++) rows.push({i, s: "row " + i});
		rows.length
	`)
	if err != nil {
		t.Fatal(err)
	}

	run := func() {
		s, err := rt.NewScope()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RunProgram(p); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}

	run()
	after := rt.Stats().Cells
	for i := 0; i < 20; i++ {
		run()
	}
	end := rt.Stats().Cells

	// Each run allocates thousands of cells. If the region were not being
	// reclaimed, twenty more runs would show it.
	if end > after*2 {
		t.Fatalf("memory grew across scopes: %d cells after one run, %d after twenty-one", after, end)
	}
}

func TestScopeCloseIsIdempotent(t *testing.T) {
	rt := goant.New()
	defer rt.Close()

	s, _ := rt.NewScope()
	s.Close()
	s.Close()
}

// --- pool -------------------------------------------------------------------

func TestPoolRunsJobs(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{
		New: func() (*goant.Runtime, error) {
			rt := goant.New()
			return rt, rt.Set("prefix", "out:")
		},
	})
	defer pool.Close()

	for i := 0; i < 5; i++ {
		var got string
		err := pool.Do(context.Background(), func(s *goant.Scope) error {
			if err := s.Set("n", i); err != nil {
				return err
			}
			v, err := s.RunString(`prefix + (n * 2)`)
			if err != nil {
				return err
			}
			got = v.String()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := "out:" + itoa(i*2); got != want {
			t.Fatalf("job %d = %q, want %q", i, got, want)
		}
	}

	if st := pool.Stats(); st.Idle != 1 || st.Live != 1 {
		t.Fatalf("one runtime should have served every job, got %+v", st)
	}
}

func TestPoolIsolatesJobs(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	defer pool.Close()

	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		_, err := s.RunString(`globalThis.sticky = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var typeofSticky string
	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		v, err := s.RunString(`typeof sticky`)
		typeofSticky = v.String()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if typeofSticky != "undefined" {
		t.Fatalf("a pooled runtime leaked a global: %q", typeofSticky)
	}
}

func TestPoolRetiresAPollutedRuntime(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	defer pool.Close()

	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		_, err := s.RunString(`Object.prototype.tainted = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if st := pool.Stats(); st.Idle != 0 || st.Live != 0 {
		t.Fatalf("a polluted runtime must not be parked, got %+v", st)
	}

	var tainted string
	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		v, err := s.RunString(`typeof ({}).tainted`)
		tainted = v.String()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if tainted != "undefined" {
		t.Fatalf("the next job inherited a modified prototype: %q", tainted)
	}
}

func TestPoolDeadlineStopsTheScript(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := pool.Do(ctx, func(s *goant.Scope) error {
		_, err := s.RunString(`for (;;) {}`)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %#v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the script ran for %v after the deadline", elapsed)
	}
	if st := pool.Stats(); st.Live != 0 {
		t.Fatalf("an abandoned runtime must be retired, got %+v", st)
	}

	// The pool must still be usable afterwards.
	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		_, err := s.RunString(`1 + 1`)
		return err
	}); err != nil {
		t.Fatalf("the pool should recover: %v", err)
	}
}

func TestPoolRetiresOnMaxUses(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{MaxUses: 2})
	defer pool.Close()

	for i := 0; i < 2; i++ {
		if err := pool.Do(context.Background(), func(s *goant.Scope) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if st := pool.Stats(); st.Live != 0 {
		t.Fatalf("the runtime should have been retired after two uses, got %+v", st)
	}
}

func TestPoolIsConcurrencySafe(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{MaxIdle: 4})
	defer pool.Close()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				err := pool.Do(context.Background(), func(s *goant.Scope) error {
					if err := s.Set("w", w); err != nil {
						return err
					}
					v, err := s.RunString(`w * 2`)
					if err != nil {
						return err
					}
					if v.Int() != int64(w*2) {
						return errors.New("wrong answer; a job saw another job's globals")
					}
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if st := pool.Stats(); st.Idle > 4 {
		t.Fatalf("MaxIdle should cap what is parked, got %+v", st)
	}
}

func TestPoolAfterClose(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	pool.Close()
	err := pool.Do(context.Background(), func(s *goant.Scope) error { return nil })
	if !errors.Is(err, goant.ErrPoolClosed) {
		t.Fatalf("error = %#v", err)
	}
}

func TestPoolPropagatesJobErrors(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	defer pool.Close()

	sentinel := errors.New("job failed")
	err := pool.Do(context.Background(), func(s *goant.Scope) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %#v", err)
	}

	err = pool.Do(context.Background(), func(s *goant.Scope) error {
		_, err := s.RunString(`throw new Error("from the script")`)
		return err
	})
	var jsErr *goant.Error
	if !errors.As(err, &jsErr) || !strings.Contains(jsErr.Message, "from the script") {
		t.Fatalf("error = %#v", err)
	}
}

func TestPoolRetiresAfterAPanickingJob(t *testing.T) {
	pool := goant.NewPool(goant.PoolConfig{})
	defer pool.Close()

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic should reach the caller, not be swallowed")
			}
		}()
		_ = pool.Do(context.Background(), func(s *goant.Scope) error {
			s.RunString(`globalThis.half = 1`)
			panic("job exploded")
		})
	}()

	if st := pool.Stats(); st.Live != 0 {
		t.Fatalf("a runtime a panic escaped from must be retired, got %+v", st)
	}

	// And the pool must still work, with no trace of the abandoned run.
	var leaked string
	if err := pool.Do(context.Background(), func(s *goant.Scope) error {
		v, err := s.RunString(`typeof half`)
		leaked = v.String()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if leaked != "undefined" {
		t.Fatalf("the abandoned run leaked a global: %q", leaked)
	}
}
