package engine

import (
	"strings"
	"testing"
)

// runAgentScript runs src on a Runtime with agents enabled and returns its
// completion value as a string.
func runAgentScript(t *testing.T, src string) string {
	t.Helper()
	rt := New()
	rt.EnableAgents()
	sc, err := rt.CompileScript("agents_test.js", src)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	v, err := rt.RunScript(sc)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.DrainJobs()
	return rt.strGo(v)
}

// A started agent sees the same bytes, and a notify wakes it. This is the whole
// contract in one script: shared memory in, a wait, a wake, a report back.
func TestAgentWaitAndNotify(t *testing.T) {
	got := runAgentScript(t, "\n"+`
		$262.agent.start(`+"`"+`
			$262.agent.receiveBroadcast(function(sab) {
				const i32 = new Int32Array(sab);
				Atomics.add(i32, 1, 1);
				const status = Atomics.wait(i32, 0, 0);
				$262.agent.report(status);
				$262.agent.leaving();
			});
		`+"`"+`);
		const i32 = new Int32Array(new SharedArrayBuffer(8));
		$262.agent.broadcast(i32.buffer);
		while (Atomics.load(i32, 1) !== 1) ;
		let woken = 0;
		while ((woken = Atomics.notify(i32, 0, 1)) === 0) ;
		let report = null;
		while ((report = $262.agent.getReport()) === null) $262.agent.sleep(1);
		report + ":" + woken;
	`)
	if got != "ok:1" {
		t.Fatalf("agent wait/notify = %q, want %q", got, "ok:1")
	}
}

// The turn is taken away, not given up. Both scripts here spin in a loop with
// nothing in its body and no call in sight, which is what the test262 harness
// does; if the only way to yield were voluntary this would never terminate.
func TestAgentPreemptsASpinLoop(t *testing.T) {
	got := runAgentScript(t, "\n"+`
		$262.agent.start(`+"`"+`
			$262.agent.receiveBroadcast(function(sab) {
				const i32 = new Int32Array(sab);
				while (Atomics.load(i32, 0) !== 1) ;
				Atomics.store(i32, 1, 1);
				$262.agent.leaving();
			});
		`+"`"+`);
		const i32 = new Int32Array(new SharedArrayBuffer(8));
		$262.agent.broadcast(i32.buffer);
		Atomics.store(i32, 0, 1);
		while (Atomics.load(i32, 1) !== 1) ;
		"through";
	`)
	if got != "through" {
		t.Fatalf("spin-loop preemption = %q, want %q", got, "through")
	}
}

// Several agents queue on one location and are released one at a time, in the
// order they arrived: the [[WaiterList]] is FIFO.
func TestAgentWaiterListIsFIFO(t *testing.T) {
	got := runAgentScript(t, "\n"+`
		const N = 3;
		for (let n = 0; n < N; n++) {
			$262.agent.start(`+"`"+`
				$262.agent.receiveBroadcast(function(sab) {
					const i32 = new Int32Array(sab);
					// One at a time, so the arrival order is the report order.
					while (Atomics.compareExchange(i32, 2, 0, 1) !== 0) ;
					$262.agent.report(String(`+"${n}"+`));
					Atomics.wait(i32, 0, 0);
					$262.agent.report("woke" + `+"${n}"+`);
					$262.agent.leaving();
				});
			`+"`"+`);
		}
		const i32 = new Int32Array(new SharedArrayBuffer(16));
		$262.agent.broadcast(i32.buffer);

		function nextReport() {
			let r = null;
			while ((r = $262.agent.getReport()) === null) $262.agent.sleep(1);
			return r;
		}

		const arrived = [];
		for (let i = 0; i < N; i++) {
			while (Atomics.load(i32, 2) !== 1) ;
			arrived.push(nextReport());
			$262.agent.sleep(20);   // let it reach the wait
			Atomics.store(i32, 2, 0);
		}
		const woke = [];
		for (let i = 0; i < N; i++) {
			while (Atomics.notify(i32, 0, 1) === 0) ;
			woke.push(nextReport());
		}
		arrived.join(",") + "|" + woke.join(",");
	`)
	parts := strings.Split(got, "|")
	if len(parts) != 2 {
		t.Fatalf("unexpected result %q", got)
	}
	arrived := strings.Split(parts[0], ",")
	woke := strings.Split(parts[1], ",")
	if len(arrived) != 3 || len(woke) != 3 {
		t.Fatalf("wanted 3 of each, got %q", got)
	}
	for i := range arrived {
		if woke[i] != "woke"+arrived[i] {
			t.Fatalf("wake order %v does not match arrival order %v", woke, arrived)
		}
	}
}

// waitAsync does not block its agent: the same script goes on to notify itself,
// and the promise settles from the event loop.
func TestAgentWaitAsyncSettles(t *testing.T) {
	got := runAgentScript(t, `
		const i32 = new Int32Array(new SharedArrayBuffer(8));
		const r = Atomics.waitAsync(i32, 0, 0, 5000);
		let out = "pending";
		r.value.then(v => { out = v; });
		Atomics.notify(i32, 0, 1);
		r.async + ":" + (r.value instanceof Promise);
	`)
	if got != "true:true" {
		t.Fatalf("waitAsync shape = %q, want %q", got, "true:true")
	}
}

// A waitAsync with a deadline settles as "timed-out" even though nothing else
// is on the queue to drive the event loop.
func TestAgentWaitAsyncTimesOut(t *testing.T) {
	rt := New()
	rt.EnableAgents()
	sc, err := rt.CompileScript("agents_test.js", `
		globalThis.out = "pending";
		const i32 = new Int32Array(new SharedArrayBuffer(8));
		Atomics.waitAsync(i32, 0, 0, 1).value.then(v => { globalThis.out = v; });
	`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if _, err := rt.RunScript(sc); err != nil {
		t.Fatalf("run: %v", err)
	}
	rt.DrainJobs()
	v, _ := rt.getField(rt.global, "out")
	if got := rt.strGo(v); got != "timed-out" {
		t.Fatalf("waitAsync outcome = %q, want %q", got, "timed-out")
	}
}

// Without EnableAgents there is no $262.agent at all: starting one is a host
// capability, and a Runtime that was not given it cannot be talked into
// spawning goroutines. With no grants at all there is no $262 either.
func TestAgentsAreOffByDefault(t *testing.T) {
	rt := New()
	v, err := rt.RunString("agents_test.js", `typeof globalThis.$262 + " " + typeof globalThis.$262?.agent`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := rt.strGo(v); got != "undefined undefined" {
		t.Fatalf("$262/$262.agent on a bare Runtime = %q, want %q", got, "undefined undefined")
	}
}

// EnableAgents grants agents and nothing else. It needs somewhere to hang
// $262.agent, and the namespace it creates for that must not come with the
// capabilities EnableHostAPI grants.
func TestEnablingAgentsDoesNotGrantTheRestOfTheHostAPI(t *testing.T) {
	rt := New()
	rt.EnableAgents()
	v, err := rt.RunString("agents_test.js", `[
		typeof $262.agent,
		typeof $262.detachArrayBuffer,
		typeof $262.createRealm,
		typeof $262.evalScript,
		typeof globalThis.createRealm,
		typeof globalThis.evalScript
	].join(",")`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := rt.strGo(v), "object,undefined,undefined,undefined,undefined,undefined"; got != want {
		t.Fatalf("after EnableAgents = %q, want %q", got, want)
	}
}
