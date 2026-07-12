// extract.cjs — deterministic extraction step of the conformance corpus
// generator (see TODO 0.3). Loads the pinned kangax/compat-table data files from
// ./vendor and emits one JSON record per (feature, subtest) exec to stdout:
//
//   { dataFile, category, feature, subtest, exec, isAsync }
//
// `exec` is the raw JS body a runner must evaluate; a compat-table exec is a
// function whose body is a block comment (`function(){/* code */}`) in the
// es6+ data files, or a plain function in data-es5. `isAsync` flags execs that
// signal completion via the global `asyncTestPassed` callback.
//
// This step is node-only and hermetic (no network): the Go mapper (main.go)
// consumes its JSON to produce conformance/compat-table/<cat>/*.js.

const path = require('path');

const DATA_FILES = [
  'data-es5.js',
  'data-es6.js',
  'data-es2016plus.js',
  'data-esnext.js',
  'data-esintl.js',
];

// extractExec turns a compat-table exec function into its runnable source. The
// es6+ style hides the body in a block comment; data-es5 uses a real function.
function extractExec(fn) {
  if (typeof fn !== 'function') return null;
  const s = fn.toString();
  const open = s.indexOf('{/*');
  if (open !== -1) {
    const close = s.indexOf('*/}', open);
    if (close !== -1) return s.slice(open + 3, close);
  }
  // Plain function: take everything between the first '{' and the last '}'.
  const b = s.indexOf('{');
  const e = s.lastIndexOf('}');
  if (b !== -1 && e > b) return s.slice(b + 1, e);
  return null;
}

const records = [];

for (const df of DATA_FILES) {
  const mod = require(path.join(__dirname, 'vendor', df));
  const tests = mod.tests || [];
  for (const test of tests) {
    const feature = test.name;
    const category = test.category || '';
    const subs =
      Array.isArray(test.subtests) && test.subtests.length
        ? test.subtests
        : [{ name: null, exec: test.exec }];
    for (const sub of subs) {
      const exec = extractExec(sub.exec);
      if (exec == null) continue;
      records.push({
        dataFile: df,
        category,
        feature,
        subtest: sub.name,
        exec,
        isAsync: /asyncTestPassed/.test(exec),
      });
    }
  }
}

process.stdout.write(JSON.stringify(records, null, 2) + '\n');
process.stderr.write(`extract: ${records.length} exec records from ${DATA_FILES.length} data files\n`);
