// ES1 §15.4.5.2 — Array length
function check(a, b) { if (a !== b) throw "length: " + a + " !== " + b; }
check([1, 2, 3].length, 3);
check([].length, 0);
var a = [1, 2, 3];
a.push(4, 5);
check(a.length, 5);
a.pop();
check(a.length, 4);
check(new Array(10).length, 10);
console.log("PASS");
