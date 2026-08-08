package engine

import "testing"

func TestOpcodeTableSanity(t *testing.T) {
	if NumOpcodes != 218 {
		t.Fatalf("NumOpcodes=%d want 218", NumOpcodes)
	}
	if OpInvalid != 0 {
		t.Fatal("OpInvalid must be 0")
	}
	// Spot-check a few well-known opcodes against opcode.h.
	checks := []struct {
		op              Opcode
		name            string
		size, pop, push int
		format          OpFormat
	}{
		{OpConst, "CONST", 5, 0, 1, FmtConst},
		{OpConstI8, "CONST_I8", 2, 0, 1, FmtI8},
		{OpAdd, "ADD", 1, 2, 1, FmtNone},
		{OpGetField, "GET_FIELD", 7, 1, 1, FmtAtom},
		{OpCall, "CALL", 3, 1, 1, FmtNpop},
		{OpJmp, "JMP", 5, 0, 0, FmtLabel},
		{OpReturn, "RETURN", 1, 1, 0, FmtNone},
		{OpDefineClass, "DEFINE_CLASS", 14, 2, 2, FmtAtomU8},
	}
	for _, c := range checks {
		if c.op.Name() != c.name {
			t.Errorf("%v Name=%q want %q", c.op, c.op.Name(), c.name)
		}
		if c.op.Size() != c.size {
			t.Errorf("%s Size=%d want %d", c.name, c.op.Size(), c.size)
		}
		pop, push := c.op.StackEffect()
		if pop != c.pop || push != c.push {
			t.Errorf("%s StackEffect=(%d,%d) want (%d,%d)", c.name, pop, push, c.pop, c.push)
		}
		if c.op.Format() != c.format {
			t.Errorf("%s Format=%v want %v", c.name, c.op.Format(), c.format)
		}
	}
}

func TestOpcodeJITFlags(t *testing.T) {
	// ADD is JIT-eligible, inlineable, and needs bailout per opcode.h OP_FLAG.
	if OpAdd.Flags()&OpfJitEligible == 0 {
		t.Error("ADD should be JIT eligible")
	}
	if OpAdd.Flags()&OpfJitNeedsBailout == 0 {
		t.Error("ADD should need bailout")
	}
	// THROW is eligible but not inlineable.
	if OpThrow.Flags()&OpfJitInlineable != 0 {
		t.Error("THROW should not be inlineable")
	}
}
