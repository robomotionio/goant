// Call-heavy: frame setup and teardown dominate.
function fib(n) { return n < 2 ? n : fib(n - 1) + fib(n - 2); }
RESULT = fib(24);
