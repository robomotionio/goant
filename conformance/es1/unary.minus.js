function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(-5,-5);check(-(3),-3);check(-(-4),4);check(-0===0,true);console.log('PASS');
