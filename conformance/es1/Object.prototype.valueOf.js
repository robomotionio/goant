function check(a, b) { if (a !== b) throw "obj.valueOf: " + a + " !== " + b; }
var o = {};
check(o.valueOf() === o, true);
check(typeof ({}).valueOf, "function");
console.log("PASS");
