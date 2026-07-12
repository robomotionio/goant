// ES1 §12.7 — continue statement
function check(a, b) { if (a !== b) throw "continue: " + a + " !== " + b; }
var s = 0;
for (var i = 0; i < 10; i++) { if (i % 2 === 0) continue; s += i; }
check(s, 25);
console.log("PASS");
