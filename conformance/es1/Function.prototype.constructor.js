function check(a, b) { if (a !== b) throw "fn.ctor: " + a + " !== " + b; }
check(Function.prototype.constructor, Function);
function f() {}
check(f.constructor, Function);
console.log("PASS");
