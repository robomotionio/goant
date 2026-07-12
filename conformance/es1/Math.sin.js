function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.sin(0),0);check(Math.abs(Math.sin(Math.PI/2)-1)<1e-10,true);console.log('PASS');
