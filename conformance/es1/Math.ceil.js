function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.ceil(3.2),4);check(Math.ceil(-3.7),-3);check(Math.ceil(5),5);console.log('PASS');
