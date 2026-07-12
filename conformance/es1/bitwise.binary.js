// ES1 §11.10 — binary bitwise operators
function check(a, b) { if (a !== b) throw "bitwise: " + a + " !== " + b; }
check(5 & 3, 1);
check(5 | 2, 7);
check(5 ^ 1, 4);
check(0xF0 & 0x0F, 0);
check(0xF0 | 0x0F, 255);
check(12 ^ 10, 6);
check(-1 & 0xFF, 255);
console.log("PASS");
