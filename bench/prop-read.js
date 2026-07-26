// Monomorphic property read: the canonical inline-cache workload. Every read
// sees the same shape, so a cache would hit every time.
var obj = { a: 1, b: 2, c: 3, d: 4 };
var sum = 0;
for (var i = 0; i < 300000; i++) {
  sum += obj.a + obj.b + obj.c + obj.d;
}
RESULT = sum;
