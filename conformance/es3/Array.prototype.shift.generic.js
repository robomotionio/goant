function check(a,b){if(a!==b)throw a+" !== "+b;} var o={0:'x',length:1};check(Array.prototype.shift.call(o),'x');console.log('PASS');
