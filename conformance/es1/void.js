function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(void 0,undefined);check(void 42,undefined);check(void 'x',undefined);console.log('PASS');
