function check(a, b) { if (a !== b) throw "toUTCString: '" + a + "' !== '" + b + "'"; }
check(new Date(0).toUTCString(), "Thu, 01 Jan 1970 00:00:00 GMT");
console.log("PASS");
