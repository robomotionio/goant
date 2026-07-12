function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(~0,-1);check(~5,-6);check(~-1,0);console.log('PASS');
