function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(2147483647+1,2147483648);check(1e10,10000000000);check(0,0);console.log('PASS');
