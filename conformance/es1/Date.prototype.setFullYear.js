function check(a, b) { if (a !== b) throw "setFullYear: " + a + " !== " + b; }
var d = new Date(Date.UTC(2000, 5, 15));
d.setFullYear(2025);
check(d.getUTCFullYear(), 2025);
check(d.getUTCMonth(), 5);
check(d.getUTCDate(), 15);
console.log("PASS");
