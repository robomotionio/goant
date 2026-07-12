function check(a, b) { if (a !== b) throw "obj.toString: '" + a + "' !== '" + b + "'"; }
check(({}).toString(), "[object Object]");
check(Object.prototype.toString.call([]), "[object Array]");
check(Object.prototype.toString.call(null), "[object Null]");
check(Object.prototype.toString.call(undefined), "[object Undefined]");
check(Object.prototype.toString.call(42), "[object Number]");
check(Object.prototype.toString.call("s"), "[object String]");
console.log("PASS");
