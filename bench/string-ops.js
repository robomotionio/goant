// Concatenation, charCodeAt and indexOf. One unit = one iteration.
bench(function () {
  var s = "", sum = 0;
  for (var i = 0; i < 20000; i++) {
    s = "abcdefghij" + (i % 10);
    sum += s.charCodeAt(i % 10) + s.indexOf("f");
  }
  return sum;
}, 20000);
