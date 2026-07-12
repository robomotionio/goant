function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Number.MIN_VALUE>0,true);check(Number.MIN_VALUE<1e-307,true);console.log('PASS');
