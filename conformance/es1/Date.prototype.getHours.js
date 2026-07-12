function check(a, b) { if (a !== b) throw "getHours: " + a + " !== " + b; }
check(new Date(2020, 0, 1, 13).getHours(), 13);
check(new Date(Date.UTC(2020, 0, 1, 7)).getUTCHours(), 7);
console.log("PASS");
