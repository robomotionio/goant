// Allocation and shape transitions: each literal walks the same shape chain.
// One unit = one object.
bench(function () {
  var last = null, sum = 0;
  for (var i = 0; i < 50000; i++) {
    last = { a: i, b: i + 1, c: i + 2 };
    sum += last.b;
  }
  return sum + last.a;
}, 50000);
