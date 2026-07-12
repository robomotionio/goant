// ES1 §15.4.4.2 — Array.prototype.toString
function check(a, b) { if (a !== b) throw "toString: " + a + " !== " + b; }
check([1, 2, 3].toString(), "1,2,3");
check([].toString(), "");
check(String([1, 2, 3]), "1,2,3");
console.log("PASS");
