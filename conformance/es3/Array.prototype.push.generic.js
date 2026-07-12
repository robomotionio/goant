function check(a,b){if(a!==b)throw a+" !== "+b;} var o={length:0};check(Array.prototype.push.call(o,'x'),1);check(o[0],'x');check(o.length,1);console.log('PASS');
