// ES1 §15.8.2.14 — Math.random returns [0, 1)
for (var i = 0; i < 1000; i++) {
  var r = Math.random();
  if (typeof r !== "number") throw "random not a number";
  if (r < 0 || r >= 1) throw "random out of range: " + r;
}
console.log("PASS");
