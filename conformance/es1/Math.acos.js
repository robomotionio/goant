function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.acos(1),0);check(Math.abs(Math.acos(0)-Math.PI/2)<1e-10,true);console.log('PASS');
