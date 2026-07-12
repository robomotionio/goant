// ES1 — >>>= compound assignment
function check(a, b) { if (a !== b) throw "ushra: " + a + " !== " + b; }
var x = -1;
x >>>= 28; check(x, 15);
console.log("PASS");
