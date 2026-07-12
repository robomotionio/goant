function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(+5,5);check(+'42',42);check(+true,1);check(+'',0);console.log('PASS');
