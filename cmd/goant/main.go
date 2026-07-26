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
	"runtime/pprof"
	"strings"

	"github.com/robomotionio/goant/internal/engine"
)

func main() {
	var (
		eval    = flag.String("e", "", "evaluate code from the command line")
		parse   = flag.Bool("parse", false, "parse only; report syntax errors")
		disasm  = flag.Bool("disasm", false, "disassemble compiled bytecode")
		module  = flag.Bool("module", false, "run the file as a Module (strict, this===undefined)")
		prelude = flag.String("prelude", "", "comma-separated script files to run (as scripts) before the module")
		modBase = flag.String("module-base", "", "directory that import specifiers resolve against (for scripts using import())")
		cpuProf = flag.String("cpuprofile", "", "write a CPU profile to this file")
	)
	flag.Parse()

	if *cpuProf != "" {
		f, err := os.Create(*cpuProf)
		if err != nil {
			fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fatal(err)
		}
		// Every exit below goes through os.Exit, which skips defers, so the
		// profile has to be flushed on the way out of each of them instead.
		defer stopProfile()
		profiling = true
	}

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
	if *modBase != "" {
		rt.SetModuleBase(*modBase)
	}

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
	case *module:
		// Run prelude scripts (harness globals) into the same realm first.
		if *prelude != "" {
			for _, pf := range strings.Split(*prelude, ",") {
				if pf == "" {
					continue
				}
				data, err := os.ReadFile(pf)
				if err != nil {
					fatal(err)
				}
				if _, err := rt.RunString(pf, string(data)); err != nil {
					reportAndExit(err)
				}
			}
		}
		if _, err := rt.RunModule(name, src); err != nil {
			reportAndExit(err)
		}
	default:
		if _, err := rt.RunString(name, src); err != nil {
			reportAndExit(err)
		}
	}
}

// profiling records whether a CPU profile is running, so the exit paths know
// they have to flush it before os.Exit discards the deferred stop.
var profiling bool

func stopProfile() {
	if profiling {
		pprof.StopCPUProfile()
		profiling = false
	}
}

func reportAndExit(err error) {
	stopProfile()
	var exit *engine.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func fatal(err error) {
	stopProfile()
	fmt.Fprintln(os.Stderr, "goant:", err)
	os.Exit(1)
}
