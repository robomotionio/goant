function check(a, b) { if (a !== b) throw "arr.ctor: " + a + " !== " + b; }
check([].constructor, Array);
check(Array.prototype.constructor, Array);
check([1,2,3].constructor === Array, true);
console.log("PASS");
