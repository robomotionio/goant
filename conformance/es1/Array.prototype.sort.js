// ES1 §15.4.4.11 — Array.prototype.sort
function check(a, b) { if (a !== b) throw "sort: " + a + " !== " + b; }
check([3, 1, 2].sort().join(","), "1,2,3");
check(["b", "a", "c"].sort().join(","), "a,b,c");
check([10, 2, 1].sort().join(","), "1,10,2");
check([3, 1, 2].sort(function(a, b){ return a - b; }).join(","), "1,2,3");
check([3, 1, 2].sort(function(a, b){ return b - a; }).join(","), "3,2,1");
console.log("PASS");
