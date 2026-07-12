function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.atan2(0,1),0);check(Math.abs(Math.atan2(1,1)-Math.PI/4)<1e-10,true);console.log('PASS');
