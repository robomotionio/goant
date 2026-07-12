function check(a, b) { if (a !== b) throw "getMonth: " + a + " !== " + b; }
check(new Date(2020, 0, 1).getMonth(), 0);
check(new Date(2020, 11, 1).getMonth(), 11);
check(new Date(Date.UTC(2020, 5, 1)).getUTCMonth(), 5);
console.log("PASS");
