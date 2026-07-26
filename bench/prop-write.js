// Monomorphic property write to an existing slot.
var obj = { a: 0, b: 0, c: 0, d: 0 };
for (var i = 0; i < 300000; i++) {
  obj.a = i; obj.b = i + 1; obj.c = i + 2; obj.d = i + 3;
}
RESULT = obj.a + obj.b + obj.c + obj.d;
