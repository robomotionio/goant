function check(a, b) { if (a !== b) throw "ToUint32: " + a + " !== " + b; }
check(-1 >>> 0, 4294967295);
check(4294967296 >>> 0, 0);
check(4294967297 >>> 0, 1);
check(3.9 >>> 0, 3);
check(NaN >>> 0, 0);
console.log("PASS");
