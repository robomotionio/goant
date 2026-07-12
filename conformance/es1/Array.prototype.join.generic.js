function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var o={0:'a',1:'b',length:2};check(Array.prototype.join.call(o,'-'),'a-b');console.log('PASS');
