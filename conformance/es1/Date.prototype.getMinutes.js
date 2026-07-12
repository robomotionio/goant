function check(a, b) { if (a !== b) throw "getMinutes: " + a + " !== " + b; }
check(new Date(2020, 0, 1, 0, 45).getMinutes(), 45);
check(new Date(Date.UTC(2020, 0, 1, 0, 30)).getUTCMinutes(), 30);
console.log("PASS");
