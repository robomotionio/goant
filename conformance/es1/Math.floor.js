function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.floor(3.7),3);check(Math.floor(-3.2),-4);check(Math.floor(5),5);console.log('PASS');
