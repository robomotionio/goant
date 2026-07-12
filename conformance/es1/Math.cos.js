function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.cos(0),1);check(Math.abs(Math.cos(Math.PI))+1<1e-10+2,true);console.log('PASS');
