function check(a, b) { if (a !== b) throw "strlit: '" + a + "' !== '" + b + "'"; }
check("hello", "hello");
check('single', 'single');
check("a\tb", "a\tb");
check("line1\nline2".length, 11);
check("quote\"inside", 'quote"inside');
check("", "");
check("\\", "\\");
console.log("PASS");
