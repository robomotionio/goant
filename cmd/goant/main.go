// Command goant is the CLI entry point for the pure-Go ant port.
//
//	goant file.js        run a script
//	goant -e '<code>'    evaluate a string
//	goant --parse f.js   parse only, report syntax errors (Phase 1)
//	goant --disasm f.js   disassemble compiled bytecode (Phase 3)
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"goant/internal/engine"
)

func main() {
	var (
		eval   = flag.String("e", "", "evaluate code from the command line")
		parse  = flag.Bool("parse", false, "parse only; report syntax errors")
		disasm = flag.Bool("disasm", false, "disassemble compiled bytecode")
	)
	flag.Parse()

	var src, name string
	switch {
	case *eval != "":
		src, name = *eval, "<eval>"
	case flag.NArg() >= 1:
		name = flag.Arg(0)
		data, err := os.ReadFile(name)
		if err != nil {
			fatal(err)
		}
		src = string(data)
	default:
		fmt.Fprintln(os.Stderr, "usage: goant [flags] file.js")
		flag.PrintDefaults()
		os.Exit(2)
	}

	rt := engine.New()

	switch {
	case *parse:
		if err := engine.ParseOnly(name, src); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case *disasm:
		if err := engine.DisasmOnly(name, src); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	default:
		if _, err := rt.RunString(name, src); err != nil {
			var exit *engine.ExitError
			if errors.As(err, &exit) {
				os.Exit(exit.Code)
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "goant:", err)
	os.Exit(1)
}
