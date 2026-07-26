// Iterator protocol: an IteratorResult object per step unless the engine
// recognises the array fast path. One unit = one element.
bench(function () {
  var a = new Array(2000);
  for (var i = 0; i < 2000; i++) a[i] = i;
  var sum = 0;
  for (var r = 0; r < 50; r++) {
    for (var v of a) sum += v;
  }
  return sum;
}, 100000);
