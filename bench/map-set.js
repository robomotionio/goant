// Map/Set hashing and lookup.
var m = new Map();
for (var i = 0; i < 100000; i++) m.set(i, i * 2);
var sum = 0;
for (var r = 0; r < 3; r++) {
  for (var i = 0; i < 100000; i++) sum += m.get(i);
}
RESULT = sum;
