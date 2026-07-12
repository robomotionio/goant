function check(a,b){if(a!==b)throw a+" !== "+b;} var o={0:'a',length:1};check(Array.prototype.pop.call(o),'a');check(o.length,0);console.log('PASS');
