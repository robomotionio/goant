function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.LN2-0.6931471805599453)<1e-15,true);console.log('PASS');
