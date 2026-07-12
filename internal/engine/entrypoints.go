package engine

import "fmt"

// CLI entry points. These are thin wrappers that gain real behavior as the
// front end (Phase 1) and compiler (Phase 3) land.

// ParseOnly parses src and reports any syntax error (goant --parse).
func ParseOnly(filename, src string) error {
	_, err := Parse(filename, src)
	return err
}

// DisasmOnly compiles src and writes a bytecode listing to stdout
// (goant --disasm).
func DisasmOnly(filename, src string) error {
	rt := New()
	prog, err := Parse(filename, src)
	if err != nil {
		return err
	}
	fn, err := rt.Compile(prog, filename, src)
	if err != nil {
		return err
	}
	fmt.Print(rt.Disassemble(fn))
	return nil
}
