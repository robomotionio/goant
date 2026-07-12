function check(a, b) { if (a !== b) throw "ToBoolean: " + a + " !== " + b; }
check(!!0, false); check(!!1, true); check(!!"", false); check(!!"x", true);
check(!!null, false); check(!!undefined, false); check(!!NaN, false);
check(!!{}, true); check(!![], true); check(!!-0, false);
console.log("PASS");
