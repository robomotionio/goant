// Command goja-run evaluates a JavaScript file under goja, presenting the same
// command-line contract as ./goant so both goant-bench and goant-t262 can drive
// either engine and the numbers mean the same thing.
//
// Only the host hooks those two harnesses need are installed — console, print,
// evalScript — because the point is to measure the engine, not a runtime built
// on top of one. Anything goja cannot do (ES modules) exits non-zero rather
// than being emulated, so the score reflects the engine's real coverage.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dop251/goja"
)

func main() {
	var (
		module = flag.Bool("module", false, "run the file as an ES module")
		_      = flag.String("module-base", "", "directory import specifiers resolve against (ignored)")
		_      = flag.String("prelude", "", "script to run before the module (ignored)")
	)
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: goja-run [flags] script.js")
		os.Exit(2)
	}
	if *module {
		// goja has no ES module goal. Saying so is the honest result; emulating
		// it with a script evaluation would report coverage that is not there.
		fmt.Fprintln(os.Stderr, "goja-run: ES modules are not supported")
		os.Exit(1)
	}

	path := flag.Arg(0)
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	vm := goja.New()
	install(vm)

	if _, err := vm.RunScript(path, string(src)); err != nil {
		// A thrown value must reach stderr with its constructor name intact:
		// that is what a negative test is matched on. goja's Exception.String
		// renders "TypeError: …" plus a stack, which is the same shape goant
		// produces, and neither prints the word the harness uses to tell a
		// parse-time error from a runtime one.
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func install(vm *goja.Runtime) {
	writeLine := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		fmt.Println(strings.Join(parts, " "))
		return goja.Undefined()
	}

	// print is what test262's doneprintHandle.js signals completion through.
	vm.Set("print", writeLine)

	console := vm.NewObject()
	console.Set("log", writeLine)
	console.Set("error", writeLine)
	console.Set("warn", writeLine)
	console.Set("info", writeLine)
	vm.Set("console", console)

	// evalScript is indirect, global-scope eval. The test262 host object $262
	// references it eagerly, so it has to exist before anything else runs.
	vm.Set("evalScript", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		v, err := vm.RunString(call.Arguments[0].String())
		if err != nil {
			panic(vm.ToValue(err.Error()))
		}
		return v
	})

	// createRealm has no goja counterpart. It is referenced only from inside
	// $262.createRealm, so throwing here fails exactly the tests that use it.
	vm.Set("createRealm", func(call goja.FunctionCall) goja.Value {
		panic(vm.NewTypeError("createRealm is not supported"))
	})
}
