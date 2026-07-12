function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(-5),5);check(Math.abs(5),5);check(Math.abs(0),0);check(Math.abs(-3.5),3.5);console.log('PASS');
