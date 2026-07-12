// ES1 §11.13 — assignment operators
function check(a, b) { if (a !== b) throw "assignment: " + a + " !== " + b; }
var x = 5;
x += 3; check(x, 8);
x -= 2; check(x, 6);
x *= 4; check(x, 24);
x /= 3; check(x, 8);
x %= 5; check(x, 3);
check((x = 10), 10);
var y = 1; y = x = 7; check(y, 7); check(x, 7);
console.log("PASS");
