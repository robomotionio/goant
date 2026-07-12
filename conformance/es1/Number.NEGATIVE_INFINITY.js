function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Number.NEGATIVE_INFINITY,-Infinity);console.log('PASS');
