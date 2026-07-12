// ES1 §12.6.3 — for statement
function check(a, b) { if (a !== b) throw "for: " + a + " !== " + b; }
var sum = 0;
for (var i = 0; i < 5; i++) sum += i;
check(sum, 10);
var p = 1;
for (var j = 1; j <= 5; j++) { p *= j; }
check(p, 120);
var count = 0;
for (;;) { count++; if (count >= 3) break; }
check(count, 3);
console.log("PASS");
