package goant_test

import (
	"context"
	"testing"
	"time"

	"github.com/robomotionio/goant"
)

// churn is script that forces the collector to run and then reuses what it
// freed: garbage past the collection floor (16384 live objects), collected at
// the interpreter's next safepoint, then live objects allocated into the freed
// slots. A Value nothing was holding now names one of those.
const churn = `
	for (let i = 0; i < 60000; i++) ({ garbage: i });
	globalThis.junk = [];
	for (let i = 0; i < 20000; i++) globalThis.junk.push({ i, s: 'x' + i });
`

// A promise only Go is holding survives the loop that settles it.
//
// A script hands a promise to its host and keeps no reference of its own —
// `return (async () => …)()` is the everyday shape. The promise settles, the
// loop goes on running other work, and a collection in that work found nothing
// pointing at the promise: it was swept, its slot was reused, and the Value the
// host was awaiting named whatever was allocated next. go-deepseek read back ""
// (and once a Proxy getter) where a JSON answer should have been, on every
// second turn of a long-lived Runtime. AwaitContext now holds what it awaits.
func TestAwaitContextHoldsItsPromise(t *testing.T) {
	rt := goant.New(goant.WithRealTimers(true))
	defer rt.Close()
	// churnLater is defined at the top level on purpose: a callback written
	// inside start() would close over start's scope, and with it the promise.
	if _, err := rt.RunString(`
		globalThis.churnLater = () => setTimeout(() => {` + churn + `}, 20);
		globalThis.sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
		// An async function's promise is held by its suspended frame, and by
		// nothing once the function returns — the shape go-deepseek's turn had.
		globalThis.start = async () => {
			churnLater(); // after it settles, while the loop is still running
			await sleep(0);
			return 'kept';
		};
	`); err != nil {
		t.Fatal(err)
	}
	start, err := rt.Get("start")
	if err != nil {
		t.Fatal(err)
	}
	promise, err := start.Function().Call()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := rt.AwaitContext(ctx, promise)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if got.String() != "kept" {
		t.Fatalf("the awaited promise was swept: read back %q (%s)", got.String(), got.TypeOf())
	}
	if !promise.IsPromise() {
		t.Fatal("the host's handle no longer names a promise")
	}
}

// Await holds its promise too: draining the queue runs JavaScript, and
// JavaScript can collect.
func TestAwaitHoldsItsPromise(t *testing.T) {
	rt := goant.New()
	defer rt.Close()
	// As above, the churn is outside the scope that holds the promise.
	if _, err := rt.RunString(`globalThis.churnSoon = () =>
		Promise.resolve().then(() => {}).then(() => {}).then(() => {` + churn + `});`); err != nil {
		t.Fatal(err)
	}
	promise, err := rt.RunString(`(async () => {
		churnSoon(); // queued behind the settlement, so the sweep lands after it
		await null;
		return 'kept';
	})()`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rt.Await(promise)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if got.String() != "kept" {
		t.Fatalf("the awaited promise was swept: read back %q (%s)", got.String(), got.TypeOf())
	}
}

// Hold keeps any Value alive across JavaScript that collects, until released.
func TestHoldKeepsAValueAlive(t *testing.T) {
	rt := goant.New()
	defer rt.Close()
	v, err := rt.RunString(`({ name: 'kept' })`)
	if err != nil {
		t.Fatal(err)
	}
	release := rt.Hold(v)
	rt.Collect()
	if _, err := rt.RunString(churn); err != nil {
		t.Fatal(err)
	}
	name, err := v.Object().Get("name")
	if err != nil || name.String() != "kept" {
		t.Fatalf("a held object was swept: name %q, %v", name.String(), err)
	}
	release()
	release() // idempotent

	// Hold on a closed Runtime is a no-op that still returns a callable release.
	closed := goant.New()
	closed.Close()
	closed.Hold(v)()
}
