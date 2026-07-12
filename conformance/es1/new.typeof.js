function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} function F(){}check(typeof new F(),'object');check(typeof new Object(),'object');console.log('PASS');
