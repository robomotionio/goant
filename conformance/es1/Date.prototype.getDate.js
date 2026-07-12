function check(a, b) { if (a !== b) throw "getDate: " + a + " !== " + b; }
check(new Date(2020, 0, 15).getDate(), 15);
check(new Date(2020, 0, 1).getDate(), 1);
check(new Date(Date.UTC(2020, 0, 28)).getUTCDate(), 28);
console.log("PASS");
