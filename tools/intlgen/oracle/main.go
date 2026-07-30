// Command oracle runs a script under the V8 build the robot shipped through
// 26.7.4 (robomotionio/v8go, V8 14.7 with ICU) and prints the value of its last
// expression. It is the reference implementation the generated Intl tables and
// the conformance/v8diff expectations are taken from.
//
// It lives in its own module because it needs cgo and a ~200 MB prebuilt libv8,
// which is exactly the dependency goant exists to avoid; keeping it out of the
// parent module means `go build ./...` at the repo root never reaches it.
package main

import (
	"fmt"
	"os"

	v8 "github.com/robomotionio/v8go"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: oracle <script.js>")
		os.Exit(2)
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	iso := v8.NewIsolate()
	ctx := v8.NewContext(iso)
	// The probes build an `out` array rather than printing, so the same file can
	// be fed to goant's CLI (which needs a print) and to this.
	val, err := ctx.RunScript(string(src)+"\nout.join(\"\\n\")", "probe.js")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(val.String())
}
