function check(a, b) { if (a !== b) throw "split: " + a + " !== " + b; }
check("a,b,c".split(",").length, 3);
check("a,b,c".split(",")[1], "b");
check("hello".split("").length, 5);
check("hello".split("")[0], "h");
check("no-sep".split(",").length, 1);
check("a-b-c".split("-").join(","), "a,b,c");
console.log("PASS");
