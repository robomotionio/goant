function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(5%3,2);check(-5%3,-2);check(5.5%2,1.5);console.log('PASS');
