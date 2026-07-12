// ES1 §15.5.4.7 — String.prototype.indexOf
function check(a, b) { if (a !== b) throw "indexOf: " + a + " !== " + b; }
check("hello world".indexOf("world"), 6);
check("hello".indexOf("l"), 2);
check("hello".indexOf("z"), -1);
check("hello".indexOf(""), 0);
check("abcabc".indexOf("bc"), 1);
console.log("PASS");
