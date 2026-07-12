// ES1 §15.6 — Boolean
function check(a, b) { if (a !== b) throw "Boolean: " + a + " !== " + b; }
check(Boolean(1), true);
check(Boolean(0), false);
check(Boolean(""), false);
check(Boolean("x"), true);
check(true.toString(), "true");
check(false.toString(), "false");
console.log("PASS");
