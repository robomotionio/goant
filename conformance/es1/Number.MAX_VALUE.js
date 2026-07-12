function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Number.MAX_VALUE>1e307,true);console.log('PASS');
