// ES1 §11.12 — conditional (ternary) operator
function check(a, b) { if (a !== b) throw "cond: " + a + " !== " + b; }
check(true ? 1 : 2, 1);
check(false ? 1 : 2, 2);
check(1 < 2 ? "yes" : "no", "yes");
check((0 ? 1 : 0 ? 2 : 3), 3);
console.log("PASS");
