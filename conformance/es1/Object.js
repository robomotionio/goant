// ES1 §15.2 — Object
function check(a, b) { if (a !== b) throw "Object: " + a + " !== " + b; }
var o = {a: 1, b: 2};
check(o.hasOwnProperty("a"), true);
check(o.hasOwnProperty("c"), false);
check(Object.keys(o).join(","), "a,b");
check(({}).toString(), "[object Object]");
check(typeof Object.getPrototypeOf(o), "object");
console.log("PASS");
