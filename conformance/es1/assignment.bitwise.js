// ES1 §11.13.2 — compound bitwise assignment
function check(a, b) { if (a !== b) throw "cbit: " + a + " !== " + b; }
var x = 12;
x &= 10; check(x, 8);
x |= 5; check(x, 13);
x ^= 1; check(x, 12);
x <<= 2; check(x, 48);
x >>= 1; check(x, 24);
console.log("PASS");
