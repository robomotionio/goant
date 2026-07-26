// Map hashing, insertion and lookup. One unit = one operation.
bench(function () {
  var m = new Map();
  for (var i = 0; i < 20000; i++) m.set(i, i * 2);
  var sum = 0;
  for (var i = 0; i < 20000; i++) sum += m.get(i);
  return sum;
}, 40000);
