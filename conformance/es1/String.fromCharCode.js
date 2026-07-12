// ES1 §15.5.3.2 — String.fromCharCode
function check(a, b) { if (a !== b) throw "fromCharCode: '" + a + "' !== '" + b + "'"; }
check(String.fromCharCode(65), "A");
check(String.fromCharCode(104, 105), "hi");
check(String.fromCharCode(), "");
console.log("PASS");
