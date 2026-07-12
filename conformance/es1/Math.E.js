function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.E-2.718281828459045)<1e-15,true);console.log('PASS');
