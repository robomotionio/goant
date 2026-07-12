function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.LOG10E-0.4342944819032518)<1e-15,true);console.log('PASS');
