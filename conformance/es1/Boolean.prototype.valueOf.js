function check(a, b) { if (a !== b) throw "bool.valueOf: " + a + " !== " + b; }
check(true.valueOf(), true);
check(false.valueOf(), false);
console.log("PASS");
