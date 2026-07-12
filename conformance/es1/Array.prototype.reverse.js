// ES1 §15.4.4.4 — Array.prototype.reverse
function check(a, b) { if (a !== b) throw "reverse: " + a + " !== " + b; }
check([1, 2, 3].reverse().join(","), "3,2,1");
check([1].reverse().join(","), "1");
check([].reverse().join(","), "");
var a = [1, 2, 3, 4];
a.reverse();
check(a.join(","), "4,3,2,1");
console.log("PASS");
