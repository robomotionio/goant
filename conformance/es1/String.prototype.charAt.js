// ES1 §15.5.4.4 — String.prototype.charAt
function check(a, b) { if (a !== b) throw "charAt: '" + a + "' !== '" + b + "'"; }
check("hello".charAt(0), "h");
check("hello".charAt(4), "o");
check("hello".charAt(10), "");
check("hello".charAt(-1), "");
console.log("PASS");
