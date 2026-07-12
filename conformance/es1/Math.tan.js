function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.tan(0),0);check(Math.abs(Math.tan(Math.PI/4)-1)<1e-10,true);console.log('PASS');
