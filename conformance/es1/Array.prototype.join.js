// ES1 §15.4.4.3 — Array.prototype.join
function check(a, b) { if (a !== b) throw "join: '" + a + "' !== '" + b + "'"; }
check([1, 2, 3].join(), "1,2,3");
check([1, 2, 3].join("-"), "1-2-3");
check([].join(","), "");
check(["a", "b"].join(""), "ab");
check([1, null, 3, undefined].join(","), "1,,3,");
check([1].join("x"), "1");
console.log("PASS");
