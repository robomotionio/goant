function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} function f(){return typeof 42;}check(f(),'number');console.log('PASS');
