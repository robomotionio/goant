function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Boolean.prototype.constructor,Boolean);check(true.constructor,Boolean);console.log('PASS');
