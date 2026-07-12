function check(a, b) { if (a !== b) throw "num.valueOf: " + a + " !== " + b; }
check((42).valueOf(), 42);
check((3.14).valueOf(), 3.14);
console.log("PASS");
