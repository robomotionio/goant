function check(a, b) { if (a !== b) throw "num.toString: '" + a + "' !== '" + b + "'"; }
check((255).toString(), "255");
check((255).toString(16), "ff");
check((8).toString(2), "1000");
check((3.14).toString(), "3.14");
check((-42).toString(), "-42");
console.log("PASS");
