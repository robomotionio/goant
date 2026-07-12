function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.pow(2,10),1024);check(Math.pow(3,2),9);check(Math.pow(4,0.5),2);console.log('PASS');
