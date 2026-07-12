function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Number.NaN!==Number.NaN,true);console.log('PASS');
