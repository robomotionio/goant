// Call-heavy: frame setup and teardown dominate. fib(20) makes 13529 calls.
function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
bench(function () { return fib(20); }, 13529);
