function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.LOG2E-1.4426950408889634)<1e-15,true);console.log('PASS');
