// ES1 §15.5.5.1 — String length
function check(a, b) { if (a !== b) throw "length: " + a + " !== " + b; }
check("hello".length, 5);
check("".length, 0);
check("a".length, 1);
check(("ab" + "cd").length, 4);
console.log("PASS");
