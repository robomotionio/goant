// ES1 §11.4.8 — bitwise NOT
function check(a, b) { if (a !== b) throw "tilde: " + a + " !== " + b; }
check(~0, -1);
check(~5, -6);
check(~-1, 0);
check(~~42, 42);
console.log("PASS");
