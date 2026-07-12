// ES1 §15.5.4.16 — String.prototype.toLowerCase / toUpperCase
function check(a, b) { if (a !== b) throw "case: '" + a + "' !== '" + b + "'"; }
check("HELLO".toLowerCase(), "hello");
check("hello".toUpperCase(), "HELLO");
check("MiXeD".toLowerCase(), "mixed");
check("MiXeD".toUpperCase(), "MIXED");
console.log("PASS");
