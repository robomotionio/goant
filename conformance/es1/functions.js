// ES1 §13 — function definitions and calls
function check(a, b) { if (a !== b) throw "functions: " + a + " !== " + b; }
function factorial(n) { return n <= 1 ? 1 : n * factorial(n - 1); }
check(factorial(5), 120);
var sq = function(x) { return x * x; };
check(sq(6), 36);
function outer() { function inner() { return 42; } return inner(); }
check(outer(), 42);
function counter() { var c = 0; return function() { return ++c; }; }
var next = counter();
check(next(), 1); check(next(), 2);
console.log("PASS");
