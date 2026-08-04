//go:build amd64 || arm64

package engine

import (
	"os"
	"strconv"

	"github.com/robomotionio/goant/internal/jitasm"
	"github.com/robomotionio/goant/internal/jitmem"
)

// The compiled call, in machine code. See jitcall.go for why.

// jitCallSpareRegs is how many operand-stack registers past the depth the site
// needs: two for the guard chain, and the first of them also carries the result
// across the refill on the way back.
//
// A call emitted deeper than the window can spare them keeps the old path,
// which costs it nothing but the speed. It also bounds the depth at seven,
// which is what makes every live slot a register here — the spill and the
// refill below are the whole operand stack rather than a window over it.
const jitCallSpareRegs = 2

// jitEmitMachineCall emits a compiled function calling a compiled function.
//
// The operands are where every call site leaves them — [this?, callee,
// arg0..argN-1] at the top of the stack — and on the fall-through to done the
// callee's result is in dst, with the register window holding the slots below
// it. Control reaches slow with nothing touched, which is what lets the old
// path stand behind this one unchanged.
//
// resume is the address the runtime re-enters this frame at once it has served
// whatever the callee wanted; the caller records it in the fixup table, because
// like every other resume address it is not known until the code has somewhere
// to live.
func jitEmitMachineCall(a *jitasm.Asm, sp, argc int, method bool, site uintptr, fixups *[]jitResumeFixup, slow, done *jitasm.Label) {
	base := sp - argc - 1
	callee := jitSlot(base)
	dst := callee
	if method {
		dst = jitSlot(base - 1)
	}
	// Two registers past the live depth, which the window has because a site
	// deeper than this is not emitted. keep also survives the refill below, so
	// it is where the callee's result waits.
	keep, tmp2 := jitSlot(sp), jitSlot(sp+1)

	// The guard, before anything is written. Identity of the callee and the
	// generation the cache was filled in, which together say that entry, upvals
	// and the closure behind them still describe this function — the same
	// argument jitResolveCallee's memo rests on, and for the same reason: a
	// handle names a cell only until the next collection.
	a.MovRegImm64(keep, uint64(site))
	a.CmpRegMem(callee, keep, int32(jitOffSiteCallee))
	a.Jcc(jitasm.CondNE, slow)
	a.MovRegImm64(tmp2, uint64(jitEpochAddr()))
	a.Mov32RegMem(tmp2, tmp2, 0)
	a.Cmp32RegMem(tmp2, keep, int32(jitOffSiteEpoch))
	a.Jcc(jitasm.CondNE, slow)

	// How many compiled frames are already on the goroutine stack. Past the
	// budget the call goes the long way round, which begins by unwinding to Go
	// and so gives the next one a stack to nest on again.
	a.MovRegMem(tmp2, jitasm.RegCtx, jitmem.CtxOffNest)
	a.CmpRegImm32(tmp2, jitMaxNest)
	a.Jcc(jitasm.CondAE, slow)

	// The callee's frame. Zero means the chain stops here, which is what bounds
	// how much memory a deep recursion leaves behind.
	a.MovRegMem(tmp2, jitasm.RegCtx, jitmem.CtxOffNext)
	a.TestRegReg(tmp2, tmp2)
	a.Jcc(jitasm.CondE, slow)
	// And that the frame that last used it did not want an operand stack of its
	// own: the callee is compiled against the inline one, and the runtime reads
	// its operands through whichever the context says.
	a.MovRegMem(jitRegScratch, tmp2, jitmem.CtxOffDeep)
	a.TestRegReg(jitRegScratch, jitRegScratch)
	a.Jcc(jitasm.CondNE, slow)

	// Everything live goes to the spill area and is declared there, exactly as a
	// helper call does it. The arguments are among it, which is what the callee
	// reads if it declines them and what the collector traces while it runs.
	for i := 0; i < sp; i++ {
		a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffSpill+int32(8*i), jitSlot(i))
	}
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, uint32(sp))

	// The callee's context. Only what changes per call: the pool, the host and
	// the operand stack were settled when the chain was built, and the constants
	// belonging to the callee are written by its own entry stub.
	a.LeaRegMem(jitRegScratch, tmp2, jitmem.CtxOffLocals)
	a.MovMemReg(tmp2, jitmem.CtxOffArgs, jitRegScratch)
	for i := 0; i < argc; i++ {
		a.MovMemReg(jitRegScratch, int32(8*i), jitSlot(base+1+i))
	}
	a.MovMemReg(tmp2, jitmem.CtxOffFnVal, callee)
	if method {
		a.MovMemReg(tmp2, jitmem.CtxOffThis, jitSlot(base-1))
	} else {
		a.MovRegImm64(jitRegScratch, uint64(mkundef()))
		a.MovMemReg(tmp2, jitmem.CtxOffThis, jitRegScratch)
	}
	a.MovRegMem(jitRegScratch, keep, int32(jitOffSiteUpvals))
	a.MovMemReg(tmp2, jitmem.CtxOffUpvals, jitRegScratch)
	// What the frame is running, so that a helper serving it can be given back
	// the function, the closure and the code it belongs to. Copied from the site
	// rather than being the site: a site is refilled and a live frame's identity
	// is not — see jitCallee. The note on ExecContext.Site is what makes this
	// store, the one pointer generated code writes, sound without a barrier.
	a.MovRegMem(jitRegScratch, keep, int32(jitOffSiteBind))
	a.MovMemReg(tmp2, jitmem.CtxOffSite, jitRegScratch)
	a.MovRegMem(jitRegScratch, jitasm.RegCtx, jitmem.CtxOffNest)
	a.AddRegImm32(jitRegScratch, 1)
	a.MovMemReg(tmp2, jitmem.CtxOffNest, jitRegScratch)

	// The live depth, raised before the callee can be collected in and lowered
	// when it is gone. Nothing between the two can run Go, so the collector
	// never sees it wrong.
	a.MovRegMem(jitRegScratch, jitasm.RegCtx, jitmem.CtxOffHost)
	a.AddMemImm32(jitRegScratch, int32(jitOffRTJitDepth), 1)

	if jitStats.enabled {
		a.MovRegImm64(jitRegScratch, uint64(jitCallFastAddr()))
		a.AddMemImm32(jitRegScratch, 0, 1)
	}
	a.MovRegMem(jitRegScratch, keep, int32(jitOffSiteEntry))
	// The context register, and on the architectures that keep the return
	// address in one, that too — the callee's own call would otherwise leave
	// this frame with no way back to the runtime.
	a.SaveLink()
	a.Push(jitasm.RegCtx)
	a.MovRegReg(jitasm.RegCtx, tmp2)
	a.CallReg(jitRegScratch)
	a.Pop(jitasm.RegCtx)
	a.RestoreLink()

	// Zero is the callee returning; anything else is the callee, or something it
	// called, wanting the runtime — and this frame is in the way of it.
	cascade := a.NewLabel()
	after := a.NewLabel()
	a.TestRegReg(jitRegExit, jitRegExit)
	a.Jcc(jitasm.CondNE, cascade)

	a.MovRegReg(keep, jitRegReturn)
	a.MovRegMem(tmp2, jitasm.RegCtx, jitmem.CtxOffHost)
	a.AddMemImm32(tmp2, int32(jitOffRTJitDepth), ^uint32(0))
	a.Bind(after)
	// The callee ran on the same registers this frame keeps its operands in, so
	// all of them come back — the two pinned ones included. Only the slots below
	// the result: the callee and its arguments are gone.
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
	end := base
	if method {
		end = base - 1
	}
	for i := 0; i < end; i++ {
		a.MovRegMem(jitSlot(i), jitasm.RegCtx, jitmem.CtxOffSpill+int32(8*i))
	}
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, 0)
	a.MovRegReg(dst, keep)
	a.Jmp(done)

	// Stepping out of the way. There is nothing to save: the operands went to
	// the spill area before the call and StackN still says so, because the
	// return path is the only one that clears it. What is left is where to come
	// back to.
	resume := a.NewLabel()
	a.Bind(cascade)
	immOff := a.MovRegImm64At(jitRegExit, 0)
	*fixups = append(*fixups, jitResumeFixup{immOff: immOff, label: resume})
	a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffResume, jitRegExit)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffExit, uint32(jitmem.ExitCallout))
	a.MovRegImm64(jitRegExit, jitmem.ExitCallout)
	a.Ret()

	// Back from the runtime, which has already popped the callee and left its
	// answer here. The depth was lowered there too, so this path does not.
	a.Bind(resume)
	a.MovRegMem(keep, jitasm.RegCtx, jitmem.CtxOffRet)
	a.Jmp(after)
}

// jitBeginCall emits the machine call ahead of the runtime path, when the site
// can have one, and returns the label the runtime path joins it at.
//
// Nil means no machine call was emitted and the site is the same code it always
// was. Two reasons: a function whose operand stack is not the context's inline
// one, which is nineteen in the Octane corpus and none of them hot, and a call
// made deeper than the register window can spare two slots for.
func jitBeginCall(a *jitasm.Asm, sites []jitCallSite, i, sp, argc int, method bool, fixups *[]jitResumeFixup, deep bool) *jitasm.Label {
	if deep || sp+jitCallSpareRegs > jitStackWindow {
		return nil
	}
	slow, done := a.NewLabel(), a.NewLabel()
	jitEmitMachineCall(a, sp, argc, method, jitSiteAddr(sites, i), fixups, slow, done)
	a.Bind(slow)
	return done
}

// jitEndCall closes a call site the machine path was emitted for.
func jitEndCall(a *jitasm.Asm, done *jitasm.Label) {
	if done != nil {
		a.Bind(done)
	}
}

// jitCountCalls is how many call sites the body has, which is how large the
// cache array must be before the first of them is emitted.
func jitCountCalls(fn *svFunc, start int) int {
	n := 0
	code := fn.code
	for ip := start; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return n
		}
		if op == OpCall || op == OpCallMethod {
			n++
		}
		ip += size
	}
	return n
}

// jitEmitMachineEntry emits the entry a compiled call site jumps to, which is
// everything a frame gets from jitRunAt when the runtime enters it instead.
//
// nargs is how many locals the caller has already written — the arity the site
// is required to match — and body is where the ordinary prologue starts, which
// checks the parameters and can still decline them.
func jitEmitMachineEntry(a *jitasm.Asm, fn *svFunc, nargs int, prologue, bail *jitasm.Label) {
	// The per-frame state the runtime writes field by field before an entry.
	// Everything the collector reads unconditionally is cleared here rather than
	// at the call site: it belongs to the callee, and one copy of it serves every
	// site that calls this function.
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffArgs+8, uint32(jitFuel))
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffArgs+16, 0)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffRet, 0)
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffStackN, 0)
	// How much of the context's locals array this frame is using, which is the
	// whole of what the collector needs to trace it — see jitCtxLocals.
	a.MovMemImm32(jitasm.RegCtx, jitmem.CtxOffNLocals, uint32(fn.maxLocals))

	// The locals the arguments did not fill. The runtime's path gets a slab
	// already set to undefined; here the array is the previous frame's at this
	// depth, so a local read before its first store would otherwise see it.
	a.MovRegMem(jitRegLocals, jitasm.RegCtx, jitmem.CtxOffArgs)
	a.MovRegImm64(jitRegGuard, uint64(nanboxPrefix))
	if nargs < fn.maxLocals {
		a.MovRegImm64(jitRegTmp, uint64(mkundef()))
		for i := nargs; i < fn.maxLocals; i++ {
			a.MovMemReg(jitRegLocals, int32(8*i), jitRegTmp)
		}
	}

	// `this`, which sloppy code does not get as it was passed: a nullish
	// receiver becomes the global object and a primitive one becomes its
	// wrapper. The first is two loads and the second is a call into the runtime,
	// so the first is done here and the second declines to the old path — which
	// coerces it and enters this function again through the runtime.
	if !fn.isStrict {
		ok := a.NewLabel()
		global := a.NewLabel()
		a.MovRegMem(jitRegTmp, jitasm.RegCtx, jitmem.CtxOffThis)
		// A Number receiver is the wrapper case. Testing taggedness first is
		// what makes the tag below meaningful at all.
		a.CmpRegReg(jitRegTmp, jitRegGuard)
		a.Jcc(jitasm.CondBE, bail)
		a.MovRegReg(jitRegScratch, jitRegTmp)
		a.ShrRegImm(jitRegScratch, nanboxTypeShift)
		a.SubRegImm32(jitRegScratch, uint32(nanboxPrefix>>nanboxTypeShift))
		a.CmpRegImm32(jitRegScratch, uint32(TUndef))
		a.Jcc(jitasm.CondE, global)
		a.CmpRegImm32(jitRegScratch, uint32(TNull))
		a.Jcc(jitasm.CondE, global)
		// One of the tags that is already an object. The shift is by CL, which
		// is why the tag is in the scratch register rather than anywhere else.
		a.MovRegImm64(jitRegTmp, 1)
		a.Shl32RegCL(jitRegTmp)
		a.AndsRegImm32(jitRegTmp, uint32(tObjectMask|1<<TTypedArray))
		a.Jcc(jitasm.CondE, bail)
		a.Jmp(ok)

		a.Bind(global)
		a.MovRegMem(jitRegTmp, jitasm.RegCtx, jitmem.CtxOffHost)
		a.MovRegMem(jitRegTmp, jitRegTmp, int32(jitOffRTGlobal))
		a.MovMemReg(jitasm.RegCtx, jitmem.CtxOffThis, jitRegTmp)
		a.Bind(ok)
	}
	a.Jmp(prologue)
}

// jitMachineCallable reports whether a compiled call site may enter this
// function directly.
//
// The restrictions are all one restriction: a frame entered this way has a
// context and nothing else. It has no vmFrame, no entry in the locals slab and
// no depth of its own in the runtime's frame counter, because publishing any of
// those means writing a Go pointer from generated code. So a function that
// needs one of them is called the way every function used to be — and what
// needs one is a body that can let its locals outlive the call (a closure over
// them, an `arguments` object aliasing them), a body that resolves names
// against something the frame carries (a direct eval, a `with`), or a tail
// call, whose whole point is to take over a frame this one does not have.
//
// mappedArgs is deliberately not among them. A mapped `arguments` object does
// alias the frame's locals, but building one takes SPECIAL_OBJ, which is on the
// list — so a function that could let its locals escape that way is refused for
// containing the opcode rather than for the flag, and the flag is true of very
// nearly every sloppy function whether it uses `arguments` or not.
func jitMachineCallable(fn *svFunc) bool {
	if fn.maxLocals <= 0 || fn.maxLocals > jitmem.InlineLocals {
		return false
	}
	if fn.dynamicVars {
		return false
	}
	code := fn.code
	for ip := fn.startIP; ip < len(code); {
		op := Opcode(code[ip])
		size := int(opTable[op].Size)
		if size <= 0 || ip+size > len(code) {
			return false
		}
		switch op {
		case OpClosure, OpSpecialObj, OpEval, OpTailCall, OpTailCallMethod,
			OpEnterWith, OpExitWith, OpWithGetVar, OpWithPutVar, OpWithDelVar:
			return false
		}
		ip += size
	}
	return true
}

// jitFillSite records what a call site called, so the next call through it is
// the machine instruction rather than the round trip.
//
// Called from the runtime path after that path has done the call, which is what
// makes every condition here a fact rather than a bet: the callee is resolved,
// its code exists, and the arguments it was given are the ones this site
// passes.
//
// The arity has to match exactly. The caller copies a fixed number of arguments
// and the callee's entry stub clears the locals from a fixed index, and neither
// knows the other's number when it is emitted — so a site that calls a function
// of a different arity keeps the old path, where the runtime reconciles the two.
func (rt *Runtime) jitFillSite(site *jitCallSite, callerStrict bool, callee Value, fn *svFunc, cl *closure) {
	if site == nil || site.dead || fn == nil || cl == nil {
		return
	}
	// The bisect knob, and it earned its place: with the whole tier on and every
	// other diagnostic silent, TypeScript failed at a call depth of three after
	// two million good calls, and halving the callees that may be entered this
	// way is what turned that into a one-line reproduction. Same hash and same
	// spelling as GOANT_JIT_MASK, which selects by the caller instead.
	if jitCallMask != ^uint64(0) && jitCallMask&(1<<jitNameBucket(fn.name)) == 0 {
		return
	}
	// mentry is the answer to jitMachineCallable, decided when the function was
	// compiled. Asking again here walked the whole body on every call that took
	// the runtime path, which was 2.2% of RayTrace.
	c := fn.jit.code
	if c == nil || c.mentry == 0 {
		return
	}
	// Strictness is the frame's, and the runtime keeps one word of it for a
	// direct eval to read. Matching it is what lets a compiled call leave that
	// word alone rather than saving and restoring it around every call.
	if fn.isStrict != callerStrict {
		return
	}
	// A closure compiled inside a `with` resolves its free names against the
	// chain it captured, and the chain is published by the frame the runtime
	// builds.
	if len(cl.capturedWith) > 0 {
		return
	}
	nargs := fn.paramCount
	if nargs > fn.maxLocals {
		nargs = fn.maxLocals
	}
	if int(site.argc) != nargs {
		return
	}
	// A fresh record rather than three stores into the old one: frames opened
	// through the previous fill are still running it. Reused when it already
	// describes this callee, which is the common refill — a collection retires
	// every site at once, and most of them fill again with what they had.
	bind := site.bind
	if bind == nil || bind.fn != fn || bind.cl != cl || bind.code != c {
		if site.rebinds >= jitSiteRebindLimit {
			return
		}
		if site.bind != nil {
			site.rebinds++
		}
		bind = &jitCallee{fn: fn, cl: cl, code: c, site: site}
	}
	// Nothing above this line has changed the site, and nothing below it can
	// decline: what generated code reads has to describe one callee, and a
	// half-written site would send a call guarded on one function into another.
	site.upvals = 0
	if len(cl.upvalues) > 0 {
		site.upvals = jitUpvalArrayAddr(cl)
	}
	site.entry = c.mentry
	site.bind = bind
	site.epoch = icEpoch()
	site.callee = callee
}

// jitRetireSite forgets what a site called.
//
// Called when the callee declined the arguments it was handed, which is the one
// answer the machine path cannot use: the frame it built is discarded and the
// call is made again through the runtime. A site that keeps doing that is
// paying for the attempt and getting nothing, so after enough of them it stops
// trying — the receiver it is called on is a shape the entry stub cannot settle,
// and no number of retries will change that.
func jitRetireSite(site *jitCallSite) {
	if site == nil {
		return
	}
	site.callee = 0
	if site.declines < jitSiteDeclineLimit {
		site.declines++
		return
	}
	site.dead = true
}

// jitSiteDeclineLimit is how many declined entries a site tolerates before it
// gives the machine path up for good, and jitSiteRebindLimit how many different
// callees it will describe before it stops changing its mind.
const (
	jitSiteDeclineLimit = 8
	jitSiteRebindLimit  = 8
)

// jitCallMask refuses to arm a call site whose callee's name does not hash into
// one of the 64 buckets it selects.
var jitCallMask = func() uint64 {
	if s := os.Getenv("GOANT_JIT_CALLMASK"); s != "" {
		if v, err := strconv.ParseUint(s, 0, 64); err == nil {
			return v
		}
	}
	return ^uint64(0)
}()
