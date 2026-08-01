package goant_test

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/robomotionio/goant"
)

func Example() {
	rt := goant.New()
	defer rt.Close()

	v, err := rt.RunString(`[1, 2, 3].map(n => n * 2).join("-")`)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(v.String())
	// Output: 2-4-6
}

// Go functions become JavaScript functions, with their arguments and results
// converted to and from their Go types.
func ExampleRuntime_Set_function() {
	rt := goant.New()
	defer rt.Close()

	rt.Set("upper", strings.ToUpper)
	rt.Set("hypot", func(a, b float64) float64 { return a*a + b*b })

	v, _ := rt.RunString(`upper("ada") + " " + hypot(3, 4)`)
	fmt.Println(v.String())
	// Output: ADA 25
}

// A Go function that returns an error throws it into the script.
func ExampleRuntime_Set_errors() {
	rt := goant.New()
	defer rt.Close()

	rt.Set("lookup", func(key string) (string, error) {
		if key != "host" {
			return "", fmt.Errorf("no such key %q", key)
		}
		return "localhost", nil
	})

	v, _ := rt.RunString(`
		try { lookup("port") } catch (e) { e.message }
	`)
	fmt.Println(v.String())
	// Output: no such key "port"
}

// ExportTo fills a Go value you already have, using the same json tags
// encoding/json would.
func ExampleValue_ExportTo() {
	rt := goant.New()
	defer rt.Close()

	v, _ := rt.RunString(`({host: "example.com", port: 8080, tags: ["a", "b"]})`)

	var cfg struct {
		Host string   `json:"host"`
		Port int      `json:"port"`
		Tags []string `json:"tags"`
	}
	if err := v.ExportTo(&cfg); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s:%d %v\n", cfg.Host, cfg.Port, cfg.Tags)
	// Output: example.com:8080 [a b]
}

// A JavaScript function can be bound to a Go signature and called like any
// other Go function.
func ExampleValue_ExportTo_function() {
	rt := goant.New()
	defer rt.Close()

	rt.RunString(`function repeat(s, n) { return s.repeat(n) }`)
	v, _ := rt.Get("repeat")

	var repeat func(string, int) string
	if err := v.ExportTo(&repeat); err != nil {
		log.Fatal(err)
	}
	fmt.Println(repeat("ab", 3))
	// Output: ababab
}

// A Go struct crosses into JavaScript as an object, with its exported methods
// available as functions.
func ExampleRuntime_Set_struct() {
	type Order struct {
		ID    string  `json:"id"`
		Total float64 `json:"total"`
	}

	rt := goant.New()
	defer rt.Close()

	rt.Set("order", Order{ID: "A-1", Total: 99.5})

	v, _ := rt.RunString(`order.id + " costs " + order.total.toFixed(2)`)
	fmt.Println(v.String())
	// Output: A-1 costs 99.50
}

// A script that never returns is stopped when its context is done.
func ExampleRuntime_WithContext() {
	rt := goant.New()
	defer rt.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	stop := rt.WithContext(ctx)
	defer stop()

	_, err := rt.RunString(`for (;;) {}`)
	fmt.Println(err)
	// Output: goant: execution interrupted
}

// Await drains the job queue and unwraps a promise, so an async script reads
// like a synchronous one.
func ExampleRuntime_Await() {
	rt := goant.New()
	defer rt.Close()

	v, err := rt.RunString(`(async () => {
		const parts = await Promise.all([1, 2, 3].map(async n => n * n));
		return parts.join(",");
	})()`)
	if err != nil {
		log.Fatal(err)
	}

	res, err := rt.Await(v)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(res.String())
	// Output: 1,4,9
}

// A Scope gives one run its own globals and reclaims everything it allocated,
// so the same Runtime can serve any number of independent jobs.
func ExampleScope() {
	rt := goant.New()
	defer rt.Close()

	prog, err := rt.Compile("transform.js", `JSON.stringify({sum: input.reduce((a, b) => a + b, 0)})`)
	if err != nil {
		log.Fatal(err)
	}

	for _, batch := range [][]int{{1, 2, 3}, {10, 20}} {
		s, err := rt.NewScope()
		if err != nil {
			log.Fatal(err)
		}
		s.Set("input", batch)

		v, err := s.RunProgram(prog)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(v.String()) // read it before Close

		s.Close()
	}
	// Output:
	// {"sum":6}
	// {"sum":30}
}

// A Pool keeps Runtimes warm and hands each job a fresh Scope on one, retiring
// any that is polluted, oversized or abandoned.
func ExamplePool() {
	pool := goant.NewPool(goant.PoolConfig{
		New: func() (*goant.Runtime, error) {
			rt := goant.New(goant.WithMemoryLimit(512 << 20))
			return rt, rt.Set("now", func() int64 { return 0 })
		},
		MaxUses:   50_000,
		MaxMemory: 256 << 20,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out []byte
	err := pool.Do(ctx, func(s *goant.Scope) error {
		if err := s.Set("msg", map[string]any{"n": 21}); err != nil {
			return err
		}
		v, err := s.RunString(`({doubled: msg.n * 2, at: now()})`)
		if err != nil {
			return err
		}
		out, _, err = v.AppendJSON(nil) // read it out before the Scope closes
		return err
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
	// Output: {"doubled":42,"at":0}
}

// JSON goes in and out as bytes, with no intermediate JavaScript string.
func ExampleRuntime_ParseJSON() {
	rt := goant.New()
	defer rt.Close()

	msg, err := rt.ParseJSON([]byte(`{"rows":[{"n":1},{"n":2}]}`))
	if err != nil {
		log.Fatal(err)
	}
	rt.Set("msg", msg)

	v, _ := rt.RunString(`({total: msg.rows.reduce((a, r) => a + r.n, 0)})`)

	out, _, err := v.AppendJSON(make([]byte, 0, 64))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
	// Output: {"total":3}
}
