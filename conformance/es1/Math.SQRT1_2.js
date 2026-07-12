function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.abs(Math.SQRT1_2-0.7071067811865476)<1e-15,true);console.log('PASS');
