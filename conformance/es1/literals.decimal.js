// ES1 §7.8.3 — decimal numeric literals
function check(a, b) { if (a !== b) throw "dec: " + a + " !== " + b; }
check(0, 0);
check(42, 42);
check(3.14, 3.14);
check(.5, 0.5);
check(5., 5);
check(1e3, 1000);
check(1.5e-2, 0.015);
check(1E2, 100);
console.log("PASS");
