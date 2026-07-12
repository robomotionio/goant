function check(a, b) { if (a !== b) throw "Function: " + a + " !== " + b; }
function add(a, b) { return a + b; }
check(add(2, 3), 5);
check(typeof add, "function");
check(add instanceof Function, true);
var f = function(x) { return x * 2; };
check(f(21), 42);
console.log("PASS");
