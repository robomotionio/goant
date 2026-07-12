function check(a, b) { if (a !== b) throw "octlit: " + a + " !== " + b; }
check(0777, 511);
check(010, 8);
check(0o17, 15);
console.log("PASS");
