function check(a, b) { if (a !== b) throw "getSeconds: " + a + " !== " + b; }
check(new Date(2020, 0, 1, 0, 0, 30).getSeconds(), 30);
check(new Date(Date.UTC(2020, 0, 1, 0, 0, 59)).getUTCSeconds(), 59);
console.log("PASS");
