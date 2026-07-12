function check(a, b) { if (a !== b) throw "ToInteger: " + a + " !== " + b; }
// via Math.trunc-like behavior of parseInt / array index
check(Math.trunc(3.9), 3);
check(Math.trunc(-3.9), -3);
check(Math.trunc(0.5), 0);
check("abcde".charAt(2.9), "c"); // index truncates
console.log("PASS");
