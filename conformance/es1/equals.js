// ES1 §11.9 — equality operators
function check(a) { if (!a) throw "equals failed"; }
check(1 == 1);
check(1 == "1");
check(null == undefined);
check(!(null == 0));
check(1 === 1);
check(!(1 === "1"));
check("a" === "a");
check(NaN !== NaN);
check(1 != 2);
check(1 !== "1");
console.log("PASS");
