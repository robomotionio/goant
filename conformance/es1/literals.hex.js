// ES1 §7.8.3 — hex integer literals
function check(a, b) { if (a !== b) throw "hex: " + a + " !== " + b; }
check(0x0, 0);
check(0xFF, 255);
check(0xff, 255);
check(0x10, 16);
check(0xDEAD, 57005);
console.log("PASS");
