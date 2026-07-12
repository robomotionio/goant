function check(a,b){if(a!==b)throw a+" !== "+b;} var o={};check(typeof o.toLocaleString(),'string');console.log('PASS');
