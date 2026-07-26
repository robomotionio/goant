// Dense integer-indexed element access, read and write.
var a = new Array(1000);
for (var i = 0; i < 1000; i++) a[i] = i;
var sum = 0;
for (var r = 0; r < 1000; r++) {
  for (var i = 0; i < 1000; i++) sum += a[i];
  a[r % 1000] = sum & 1023;
}
RESULT = sum;
