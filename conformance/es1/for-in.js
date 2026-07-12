// ES1 §12.6.4 — for-in statement
function check(a, b) { if (a !== b) throw "for-in: " + a + " !== " + b; }
var o = {a: 1, b: 2, c: 3};
var sum = 0;
for (var k in o) sum += o[k];
check(sum, 6);
var keys = "";
for (var k in o) keys += k;
check(keys, "abc");
var a = [10, 20, 30];
var t = 0;
for (var i in a) t += a[i];
check(t, 60);
var count = 0;
for (var x in {}) count++;
check(count, 0);
console.log("PASS");
