function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(typeof Function.prototype,'function');check(Function.prototype(),undefined);console.log('PASS');
