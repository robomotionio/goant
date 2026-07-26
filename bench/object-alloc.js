// Allocation and shape transitions: each literal builds the same shape chain.
var last = null, sum = 0;
for (var i = 0; i < 200000; i++) {
  last = { a: i, b: i + 1, c: i + 2 };
  sum += last.b;
}
RESULT = sum + last.a;
