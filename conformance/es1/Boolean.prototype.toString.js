function check(a, b) { if (a !== b) throw "bool.toString: " + a + " !== " + b; }
check(true.toString(), "true");
check(false.toString(), "false");
check((1 < 2).toString(), "true");
console.log("PASS");
