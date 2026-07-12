function check(a, b) { if (a !== b) throw "valueOf: " + a + " !== " + b; }
check(new Date(5000).valueOf(), 5000);
check(new Date(0).valueOf(), 0);
check(+new Date(1000), 1000);
console.log("PASS");
