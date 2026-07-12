function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check('x'.constructor,String);check(String.prototype.constructor,String);console.log('PASS');
