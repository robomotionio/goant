function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.LN10-2.302585092994046)<1e-15,true);console.log('PASS');
