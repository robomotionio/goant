// ES1 §12.8 — break statement
function check(a, b) { if (a !== b) throw "break: " + a + " !== " + b; }
var s = 0;
for (var i = 0; i < 100; i++) { if (i === 5) break; s += i; }
check(s, 10);
var n = 0;
while (true) { n++; if (n >= 7) break; }
check(n, 7);
console.log("PASS");
