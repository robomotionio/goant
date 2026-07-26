package engine

// Bytecode disassembler (ant sv_disasm, src/silver/compiler.c). Decodes a
// compiled function's bytecode into a human-readable listing, driven by the
// generated opTable operand formats. Backs `goant --disasm` and the planned
// bytecode-diff harness (TODO Phase 3).

import (
	"fmt"
	"strings"
)

// Disassemble renders fn's bytecode as a listing.
func (rt *Runtime) Disassemble(fn *svFunc) string {
	var b strings.Builder
	name := fn.name
	if name == "" {
		name = "<script>"
	}
	fmt.Fprintf(&b, "=== %s (%d bytes, %d locals, %d consts) ===\n",
		name, len(fn.code), fn.maxLocals, len(fn.constants))
	ip := 0
	for ip < len(fn.code) {
		ip = rt.disasmInstr(&b, fn, ip)
	}
	return b.String()
}

func (rt *Runtime) disasmInstr(b *strings.Builder, fn *svFunc, ip int) int {
	op := Opcode(fn.code[ip])
	fmt.Fprintf(b, "%6d  %-16s", ip, op.Name())
	code := fn.code
	next := ip + op.Size()

	switch op.Format() {
	case FmtNone:
		// no operand
	case FmtI8:
		fmt.Fprintf(b, " %d", int8(code[ip+1]))
	case FmtU8, FmtAtomU8:
		fmt.Fprintf(b, " %d", code[ip+1])
	case FmtU16, FmtLoc, FmtLoc8, FmtArg:
		fmt.Fprintf(b, " %d", readU16(code, ip+1))
	case FmtConst:
		idx := readU32(code, ip+1)
		fmt.Fprintf(b, " #%d %s", idx, rt.constRepr(fn, int(idx)))
	case FmtConst8:
		idx := code[ip+1]
		fmt.Fprintf(b, " #%d %s", idx, rt.constRepr(fn, int(idx)))
	case FmtLabel:
		// slice encoding: absolute u32 target
		fmt.Fprintf(b, " -> %d", readU32(code, ip+1))
	case FmtU32, FmtI32, FmtAtom:
		fmt.Fprintf(b, " %d", readU32(code, ip+1))
	default:
		if op.Size() > 1 {
			fmt.Fprintf(b, " (%d operand bytes)", op.Size()-1)
		}
	}
	b.WriteByte('\n')
	if next <= ip {
		next = ip + 1 // guard against zero-size opcodes
	}
	return next
}

// constRepr renders a constant-pool entry for the listing.
func (rt *Runtime) constRepr(fn *svFunc, idx int) string {
	if idx < 0 || idx >= len(fn.constants) {
		return "<oob>"
	}
	v := fn.constants[idx]
	switch v.Type() {
	case TNum:
		return numberToString(v.Number())
	case TStr:
		return fmt.Sprintf("%q", rt.strGo(v))
	case TBool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case TUndef:
		return "undefined"
	case TNull:
		return "null"
	default:
		return fmt.Sprintf("<%s>", typeName(v.Type()))
	}
}
