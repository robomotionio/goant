// ES1 §7.4 — comments
function check(a, b) { if (a !== b) throw "comments: " + a + " !== " + b; }
var x = 1; // line comment
var y /* inline */ = 2;
/* block
   comment */
check(x + y, 3);
console.log("PASS");
