function check(a, b) { if (a !== b) throw "ltr: " + a + " !== " + b; }
var a = 1, b = 2, c = 3;
a = b = c = 10;
check(a, 10); check(b, 10); check(c, 10);
console.log("PASS");
