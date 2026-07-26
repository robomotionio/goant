// Dense integer-indexed element access. One unit = one read.
bench(function () {
  var a = new Array(1000);
  for (var i = 0; i < 1000; i++) a[i] = i;
  var sum = 0;
  for (var r = 0; r < 100; r++) {
    for (var i = 0; i < 1000; i++) sum += a[i];
  }
  return sum;
}, 100000);
