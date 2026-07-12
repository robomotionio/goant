// ES1 §11.7.3 — unsigned right shift
function check(a, b) { if (a !== b) throw "ushr: " + a + " !== " + b; }
check(-1 >>> 0, 4294967295);
check(-1 >>> 28, 15);
check(256 >>> 2, 64);
check(8 >>> 1, 4);
console.log("PASS");
