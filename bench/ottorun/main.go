// Command otto-run evaluates a JavaScript file under otto, presenting the same
// command-line contract as ./goant so both goant-bench and goant-t262 can drive
// it and the numbers mean the same thing.
//
// otto is the oldest of the three pure-Go engines and the far end of the range
// goant is measured across: it targets ES5, and its regular expressions are
// Go's RE2, so lookahead and backreferences are parse errors rather than
// features. Both facts show up as failures rather than as slow columns, which
// is the point of having it here — the table should say what an engine cannot
// do as plainly as it says how fast it is.
//
// Only the host hooks the harnesses need are installed — console, print,
// evalScript — because the point is to measure the engine, not a runtime built
// on top of one. Anything otto cannot do (ES modules) exits non-zero rather
// than being emulated, so the score reflects the engine's real coverage.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/robertkrimen/otto"
)

func main() {
	var (
		module = flag.Bool("module", false, "run the file as an ES module")
		_      = flag.String("module-base", "", "directory import specifiers resolve against (ignored)")
		_      = flag.String("prelude", "", "script to run before the module (ignored)")
	)
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: otto-run [flags] script.js")
		os.Exit(2)
	}
	if *module {
		// otto has no ES module goal. Saying so is the honest result; emulating
		// it with a script evaluation would report coverage that is not there.
		fmt.Fprintln(os.Stderr, "otto-run: ES modules are not supported")
		os.Exit(1)
	}

	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	vm := otto.New()
	install(vm)

	if _, err := vm.Run(string(src)); err != nil {
		// A thrown value must reach stderr with its constructor name intact:
		// that is what a negative test is matched on. otto's error rendering is
		// "TypeError: …", the same shape goant and goja produce.
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func install(vm *otto.Otto) {
	writeLine := func(call otto.FunctionCall) otto.Value {
		parts := make([]string, len(call.ArgumentList))
		for i, a := range call.ArgumentList {
			parts[i] = a.String()
		}
		fmt.Println(strings.Join(parts, " "))
		return otto.UndefinedValue()
	}

	// print is what test262's doneprintHandle.js signals completion through.
	vm.Set("print", writeLine)

	// otto ships a console of its own, but it renders through its inspector
	// rather than String(), so a score would come back decorated. Replacing it
	// keeps one line of stdout meaning the same thing under every engine.
	console, err := vm.Object(`({})`)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range []string{"log", "error", "warn", "info"} {
		console.Set(name, writeLine)
	}
	vm.Set("console", console)

	// globalThis is ES2015 and otto is ES5, so it is absent. The harness
	// prelude reaches for it by name to park results where nothing can be
	// eliminated as dead code. Supplying it is the same class of shim as
	// print — an environment otto never had, not a language feature being
	// backfilled — and every engine is measured through the same prelude.
	if v, _ := vm.Get("globalThis"); v.IsUndefined() {
		if g, err := vm.Run(`this`); err == nil {
			vm.Set("globalThis", g)
		}
	}

	// evalScript is indirect, global-scope eval. The test262 host object $262
	// references it eagerly, so it has to exist before anything else runs.
	vm.Set("evalScript", func(call otto.FunctionCall) otto.Value {
		if len(call.ArgumentList) == 0 {
			return otto.UndefinedValue()
		}
		v, err := vm.Run(call.ArgumentList[0].String())
		if err != nil {
			panic(vm.MakeCustomError("Error", err.Error()))
		}
		return v
	})

	// createRealm has no otto counterpart. It is referenced only from inside
	// $262.createRealm, so throwing here fails exactly the tests that use it.
	vm.Set("createRealm", func(call otto.FunctionCall) otto.Value {
		panic(vm.MakeCustomError("TypeError", "createRealm is not supported"))
	})
}
