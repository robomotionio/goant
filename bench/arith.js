// Pure numeric work: the dispatch loop with almost no object model in the way.
// One unit = one loop iteration.
bench(function () {
  var x = 0;
  for (var i = 0; i < 200000; i++) x = (x + i * 3) % 1000003;
  return x;
}, 200000);
