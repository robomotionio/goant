function check(a, b) { if (a !== b) throw "getDay: " + a + " !== " + b; }
// 2020-01-01 was a Wednesday (3)
check(new Date(Date.UTC(2020, 0, 1)).getUTCDay(), 3);
check(new Date(Date.UTC(2020, 0, 5)).getUTCDay(), 0); // Sunday
console.log("PASS");
