function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.max(1,2,3),3);check(Math.max(-1,-2),-1);check(Math.max(),-Infinity);console.log('PASS');
