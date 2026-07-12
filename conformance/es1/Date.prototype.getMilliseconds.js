function check(a, b) { if (a !== b) throw "getMs: " + a + " !== " + b; }
check(new Date(1234).getMilliseconds(), 234);
check(new Date(Date.UTC(2020, 0, 1, 0, 0, 0, 500)).getUTCMilliseconds(), 500);
console.log("PASS");
