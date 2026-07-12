function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var x=5;check(++x,6);check(x--,6);check(x,5);console.log('PASS');
