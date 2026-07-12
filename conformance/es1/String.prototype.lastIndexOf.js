function check(a, b) { if (a !== b) throw "lastIndexOf: " + a + " !== " + b; }
check("abcabc".lastIndexOf("bc"), 4);
check("abcabc".lastIndexOf("z"), -1);
check("hello".lastIndexOf("l"), 3);
console.log("PASS");
