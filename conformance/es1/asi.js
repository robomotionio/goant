// ES1 §7.9 — automatic semicolon insertion
function check(a, b) { if (a !== b) throw a + " !== " + b; }
var x = 1
var y = 2
check(x + y, 3)
var z = 10
z = z + 5
check(z, 15)
function f() {
  return
  42
}
check(f(), undefined) // ASI after return
console.log("PASS")
