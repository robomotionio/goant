function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(String.prototype.charAt.call('hello',1),'e');check(String.prototype.toUpperCase.call('abc'),'ABC');console.log('PASS');
