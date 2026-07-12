// ES1 §15.5.4.5 — String.prototype.charCodeAt
function check(a, b) { if (a !== b) throw "charCodeAt: " + a + " !== " + b; }
check("A".charCodeAt(0), 65);
check("hello".charCodeAt(0), 104);
check("hello".charCodeAt(1), 101);
check("hello".charCodeAt(100) !== "hello".charCodeAt(100), true); // NaN
console.log("PASS");
