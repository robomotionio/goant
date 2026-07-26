// Polymorphic read: four shapes at one site, which a monomorphic cache misses
// every time and a polymorphic one still hits.
var objs = [{ x: 1 }, { y: 1, x: 2 }, { z: 1, w: 2, x: 3 }, { p: 1, q: 2, r: 3, x: 4 }];
var sum = 0;
for (var i = 0; i < 300000; i++) {
  sum += objs[i & 3].x;
}
RESULT = sum;
