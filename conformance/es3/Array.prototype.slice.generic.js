function check(a,b){if(a!==b)throw a+" !== "+b;} var o={0:'a',1:'b',2:'c',length:3};check(Array.prototype.slice.call(o,1).join(','),'b,c');console.log('PASS');
