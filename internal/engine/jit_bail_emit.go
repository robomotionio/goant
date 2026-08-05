//go:build amd64 || arm64

package engine

import (
	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// jitEmitBail emits the sequence that stops this frame and hands it to the
// interpreter. See jitbail.go for what a bail is and why it is cheap.
//
// It is the first half of jitCallHelper and none of the second: the same window
// spill, the same StackN, and then a RET that nothing resumes. What replaces the
// resume address is the bytecode offset — the frame is not coming back, so what
// it owes is a place in the program rather than a place in the code.
//
// sp is the depth *before* the instruction at `at` runs, so the operand stack
// this publishes is the one that instruction expects to find. Everything below
// the register window is already in the spill array, which is where it lives
// between instructions rather than only across a call, so the loop covers the
// window alone.
//
// Both stores are 32-bit into 64-bit fields, which is what every other write to
// StackN does and rests on the same fact: Go allocates the context zeroed and
// never writes the upper half of either field, so it stays zero for the life of
// the context.
// jitBailToInterpreter finishes in the interpreter a frame that compiled code
// stopped partway through. See jitbail.go for what it carries and why.
//
// Out of line, and marked so: it is reached from jitRunAt's exit switch, which
// is the hottest Go code in the tier, and an inlined body there is register
// pressure and stack frame paid by every exit rather than by the rare one that
// takes this.
//
//go:noinline
func (rt *Runtime) jitBailToInterpreter(base int, cur *jitmem.ExecContext,
	fn *svFunc, cl *closure, fnVal, this Value, args, locals []Value) (Value, *ThrowError, bool) {
	jitStats.bails++
	res := &jitResume{ip: int(cur.BailIP), locals: locals}
	if n := int(cur.StackN); n > 0 {
		res.stack = make([]Value, n)
		for i := range res.stack {
			res.stack[i] = jitSlotAt(cur, i)
		}
	}
	rt.jitDepth = base
	// The cells this frame captured go with it. The interpreter takes them over
	// as its own open upvalues, so unlike every other way out of the run loop
	// they are read before they are dropped rather than merely dropped.
	open := rt.openUpvalsAt(rt.frameDepth)
	rt.dropOpenUpvals(rt.frameDepth)
	res.openUpvals = open
	v, e := rt.runFrameBody(fn, cl, fnVal, this, args, res)
	return v, e, true
}

// jitBailSiteFrame finishes, in the interpreter, a frame a compiled call site
// opened — which is a harder thing than the frame above, because there is no
// interpreted frame under it to hand the work back to.
//
// A frame the runtime entered has one already: runFrameBody is on the Go stack
// below it, holding the locals slab, the operand buffer and the published
// vmFrame. A frame a call site opened has none of that. It lives in a context
// and nothing else, which is the whole reason a compiled call is worth what it
// is worth — it builds no frame at all.
//
// So this builds one, and the only reason that is cheap enough to be worth
// doing is that the frames reaching it are the restricted kind: jitMachineCallable
// refuses a body whose locals can outlive it, so there is nothing aliasing the
// context's Locals and copying them into a slab is sound. `arguments` is refused
// for the same reason, which is why nil is an honest argument list.
//
// The answer goes back the way an ordinary return does — the caller's context
// gets it in Ret and resumes at the address it saved on the way out — so the
// caller cannot tell that its callee finished somewhere else. See the ExitReturn
// arm for the other half of that.
//
//go:noinline
func (rt *Runtime) jitBailSiteFrame(cur *jitmem.ExecContext) (Value, *ThrowError) {
	s := jitCtxSite(cur)
	if s == nil {
		return mkundef(), rt.typeError("JIT frame chain")
	}
	fn, cl := s.fn, s.cl
	rt.frameDepth++
	if rt.frameDepth > maxFrameDepth {
		rt.frameDepth--
		return mkundef(), rt.rangeError("Maximum call stack size exceeded")
	}
	// The three pieces of caller state a frame owes back, restored the way
	// runFrame restores them. A defer rather than three returns because
	// runFrameBody below has several ways out and none of them is this one's.
	savedStrict, savedActiveNT := rt.frameStrict, rt.activeNewTarget
	defer func() {
		rt.frameDepth--
		rt.frameStrict, rt.activeNewTarget = savedStrict, savedActiveNT
	}()
	rt.frameStrict = fn.isStrict

	this, fnVal := Value(cur.This), Value(cur.FnVal)
	f := rt.publishFrame(rt.frameDepth)
	f.fn, f.cl = fn, cl
	f.fnVal, f.thisVal = fnVal, this
	// publishFrame clears, and a cleared Value is the number zero rather than
	// undefined — see the tail handover in jitRunAt, which had the same hole.
	f.varObj, f.newTarget = mkundef(), mkundef()

	// Copied out of the context rather than addressed in place. The context goes
	// back on the chain the moment this returns, and the next frame at that
	// depth writes over it; the interpreter needs an array that outlives that.
	locals := rt.frameLocals(rt.frameDepth, fn.maxLocals)
	copy(locals, jitCtxLocals(cur))
	f.locals = locals

	res := &jitResume{ip: int(cur.BailIP), locals: locals}
	if n := int(cur.StackN); n > 0 {
		res.stack = make([]Value, n)
		for i := range res.stack {
			res.stack[i] = jitSlotAt(cur, i)
		}
	}
	// No open upvalues to carry: a body that could create one is not a body a
	// call site is allowed to enter.
	return rt.runFrameBody(fn, cl, fnVal, this, nil, res)
}

func jitEmitBail(a *jitasm.Asm, sp, at int, deep bool) {
	base, off := jitStackBase(a, deep)
	for i := jitWindowBase(sp); i < sp; i++ {
		a.MovMemReg(base, off+int32(8*i), jitSlot(i))
	}
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, uint32(sp))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffBailIP, uint32(at))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitBail))
	a.MovRegImm64(jitRegExit, jitmem.ExitBail)
	a.Ret()
}
