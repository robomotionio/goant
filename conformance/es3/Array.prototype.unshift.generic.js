function check(a,b){if(a!==b)throw a+" !== "+b;} var o={0:'c',length:1};check(Array.prototype.unshift.call(o,'a','b'),3);console.log('PASS');
