// Stub for kangax compat-table data-common. compatgen only reads each test's
// name/exec/subtests; the browser-support `res` values (which reference these
// symbols) are irrelevant, so every access resolves to a benign deep proxy.
function deep() {
  return new Proxy(function () {}, { get: () => deep(), apply: () => deep() });
}
module.exports = new Proxy({}, { get: () => deep() });
