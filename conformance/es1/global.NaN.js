function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(NaN!==NaN,true);check(typeof NaN,'number');console.log('PASS');
