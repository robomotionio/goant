function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.PI-3.141592653589793)<1e-15,true);console.log('PASS');
