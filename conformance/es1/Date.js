// ES1 §15.9 — Date construction and getTime
function check(a, b) { if (a !== b) throw "Date: " + a + " !== " + b; }
var d = new Date(0);
check(d.getTime(), 0);
check(new Date(1000).getTime(), 1000);
check(new Date(2020, 0, 1).getFullYear(), 2020);
check(Date.UTC(1970, 0, 1), 0);
check(typeof Date.now(), "number");
check((new Date(2020, 5, 15)).getTime(), Date.UTC(2020, 5, 15));
console.log("PASS");
