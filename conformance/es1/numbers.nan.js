function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(NaN!==NaN,true);check(typeof NaN,'number');check(NaN+1!==NaN+1,true);console.log('PASS');
