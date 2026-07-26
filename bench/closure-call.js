// Closure invocation with an upvalue read on every call.
function makeAdder(n) { return function (x) { return x + n; }; }
var add = makeAdder(7);
var sum = 0;
for (var i = 0; i < 500000; i++) sum += add(i);
RESULT = sum;
