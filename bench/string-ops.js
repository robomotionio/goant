// Concatenation, indexOf and charCodeAt over a growing rope.
var s = "";
var sum = 0;
for (var i = 0; i < 20000; i++) {
  s = "abcdefghij" + (i % 10);
  sum += s.charCodeAt(i % 10) + s.indexOf("f");
}
RESULT = sum;
