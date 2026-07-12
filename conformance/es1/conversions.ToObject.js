function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var o=Object('x');check(typeof o,'object');console.log('PASS');
