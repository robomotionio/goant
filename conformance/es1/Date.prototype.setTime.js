function check(a, b) { if (a !== b) throw "setTime: " + a + " !== " + b; }
var d = new Date();
d.setTime(0);
check(d.getTime(), 0);
d.setTime(86400000);
check(d.getUTCDate(), 2);
console.log("PASS");
