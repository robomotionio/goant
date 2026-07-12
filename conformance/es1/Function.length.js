function check(a, b) { if (a !== b) throw "fn.length: " + a + " !== " + b; }
check((function(){}).length, 0);
check((function(a){}).length, 1);
check((function(a, b, c){}).length, 3);
function g(x, y) {}
check(g.length, 2);
console.log("PASS");
