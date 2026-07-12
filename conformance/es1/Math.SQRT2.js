function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.SQRT2-1.4142135623730951)<1e-15,true);console.log('PASS');
