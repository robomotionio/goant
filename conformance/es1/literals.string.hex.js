function check(a, b) { if (a !== b) throw "hexesc: " + a + " !== " + b; }
check("\x41", "A");
check("\x61", "a");
check("\x00".charCodeAt(0), 0);
check("\x7F".charCodeAt(0), 127);
check("A", "A");
check("é".charCodeAt(0), 233);
console.log("PASS");
