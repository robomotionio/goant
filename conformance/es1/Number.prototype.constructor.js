function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Number.prototype.constructor,Number);check((5).constructor,Number);console.log('PASS');
