// Closure invocation with an upvalue read on every call. One unit = one call.
function makeAdder(n) { return function (x) { return x + n; }; }
bench(function () {
  var add = makeAdder(7);
  var sum = 0;
  for (var i = 0; i < 100000; i++) sum += add(i);
  return sum;
}, 100000);
