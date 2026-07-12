function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(String(42),'42');check('abc'.length,3);check('a'+'b','ab');check(typeof 'x','string');console.log('PASS');
