// Iterator protocol: allocates a result object per step unless the engine
// recognises the array fast path.
var a = new Array(2000);
for (var i = 0; i < 2000; i++) a[i] = i;
var sum = 0;
for (var r = 0; r < 200; r++) {
  for (var v of a) sum += v;
}
RESULT = sum;
