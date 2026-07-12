// ES1 §15.9.5.9 — Date.prototype.getTime
function check(a, b) { if (a !== b) throw "getTime: " + a + " !== " + b; }
check(new Date(0).getTime(), 0);
check(new Date(123456789).getTime(), 123456789);
check(new Date(-1000).getTime(), -1000);
var d = new Date(0);
d.setTime(5000);
check(d.getTime(), 5000);
console.log("PASS");
