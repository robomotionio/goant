function check(a, b) { if (a !== b) throw "obj.ctor: " + a + " !== " + b; }
check(({}).constructor, Object);
check(Object.prototype.constructor, Object);
console.log("PASS");
