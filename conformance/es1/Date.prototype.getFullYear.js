// ES1 §15.9.5.10 — getFullYear
function check(a, b) { if (a !== b) throw "getFullYear: " + a + " !== " + b; }
check(new Date(2020, 0, 1).getFullYear(), 2020);
check(new Date(1999, 11, 31).getFullYear(), 1999);
check(new Date(Date.UTC(2000, 0, 1)).getUTCFullYear(), 2000);
console.log("PASS");
