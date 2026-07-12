// With TZ=UTC the offset is 0.
function check(a, b) { if (a !== b) throw "tzoffset: " + a + " !== " + b; }
check(new Date(0).getTimezoneOffset(), 0);
console.log("PASS");
