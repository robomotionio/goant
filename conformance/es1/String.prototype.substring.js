// ES1 §15.5.4.15 — String.prototype.substring
function check(a, b) { if (a !== b) throw "substring: '" + a + "' !== '" + b + "'"; }
check("hello".substring(1, 3), "el");
check("hello".substring(0, 5), "hello");
check("hello".substring(3), "lo");
check("hello".substring(3, 1), "el"); // swapped
console.log("PASS");
