// ES1 §11.14 — comma operator
function check(a, b) { if (a !== b) throw "comma: " + a + " !== " + b; }
var x = (1, 2, 3); check(x, 3);
var a = 0, b = 0;
var r = (a = 5, b = 10, a + b); check(r, 15);
console.log("PASS");
