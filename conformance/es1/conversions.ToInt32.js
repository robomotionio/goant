function check(a, b) { if (a !== b) throw "ToInt32: " + a + " !== " + b; }
check(4294967296 | 0, 0);
check(4294967297 | 0, 1);
check(-1 | 0, -1);
check(2147483648 | 0, -2147483648);
check(3.9 | 0, 3);
check(NaN | 0, 0);
check(Infinity | 0, 0);
console.log("PASS");
