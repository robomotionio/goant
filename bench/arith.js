// Pure numeric work: measures the dispatch loop with almost no object model.
var x = 0;
for (var i = 0; i < 2000000; i++) {
  x = (x + i * 3) % 1000003;
}
RESULT = x;
