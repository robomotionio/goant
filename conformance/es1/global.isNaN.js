function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(isNaN(NaN),true);check(isNaN(0/0),true);check(isNaN(42),false);check(isNaN('x'),true);console.log('PASS');
