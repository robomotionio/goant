// ES1 §7.9 — ASI in expression / block contexts
function check(a, b) { if (a !== b) throw a + " !== " + b; }
var r = (function () {
  var a = 1
  var b = 2
  return a + b
})()
check(r, 3)
var i = 0
var sum = 0
for (i = 0; i < 3; i++) {
  sum += i
}
check(sum, 3)
console.log("PASS")
