function check(a, b) { if (a !== b) throw "ToString: '" + a + "' !== '" + b + "'"; }
check(String(42), "42"); check(String(true), "true"); check(String(null), "null");
check(String(undefined), "undefined"); check(String(3.14), "3.14");
check("" + 0, "0"); check("" + -0, "0"); check("" + [1,2,3], "1,2,3");
check(String(1e21), "1e+21"); check(String(0.0001), "0.0001");
console.log("PASS");
