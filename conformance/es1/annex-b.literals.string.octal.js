function check(a, b) { if (a !== b) throw "octesc: " + a + " !== " + b; }
check("\101", "A");
check("\141", "a");
check("\0".charCodeAt(0), 0);
check("\40", " ");
console.log("PASS");
