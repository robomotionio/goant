// ES1 §11.7 — bitwise shift operators
function check(a, b) { if (a !== b) throw "shift: " + a + " !== " + b; }
check(1 << 4, 16);
check(256 >> 2, 64);
check(-8 >> 1, -4);
check(1 << 31, -2147483648);
check(5 << 0, 5);
console.log("PASS");
