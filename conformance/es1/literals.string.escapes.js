function check(a, b) { if (a !== b) throw "escapes: " + a + " !== " + b; }
check("\n".charCodeAt(0), 10);
check("\t".charCodeAt(0), 9);
check("\r".charCodeAt(0), 13);
check("\b".charCodeAt(0), 8);
check("\f".charCodeAt(0), 12);
check("\v".charCodeAt(0), 11);
check("\0".charCodeAt(0), 0);
check("\\".charCodeAt(0), 92);
check("\"".charCodeAt(0), 34);
console.log("PASS");
