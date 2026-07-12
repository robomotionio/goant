// ES1 §15.7 — Number
function check(a, b) { if (a !== b) throw "Number: " + a + " !== " + b; }
check(Number("42"), 42);
check(Number(""), 0);
check(Number(true), 1);
check(Number.MAX_SAFE_INTEGER, 9007199254740991);
check((255).toString(16), "ff");
check((3.14159).toFixed(2), "3.14");
check(typeof Number.POSITIVE_INFINITY, "number");
console.log("PASS");
