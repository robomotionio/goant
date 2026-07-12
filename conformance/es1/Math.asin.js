function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.asin(0),0);check(Math.abs(Math.asin(1)-Math.PI/2)<1e-10,true);console.log('PASS');
