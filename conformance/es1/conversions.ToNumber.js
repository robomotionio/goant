// ES1 §9.3 — ToNumber
function check(a, b) { if (a !== b && !(a !== a && b !== b)) throw "ToNumber: " + a + " !== " + b; }
check(+"42", 42);
check(+"", 0);
check(+"  10  ", 10);
check(+true, 1);
check(+false, 0);
check(+null, 0);
check(+"0x1F", 31);
check(+undefined !== +undefined, true); // NaN
console.log("PASS");
