function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.prototype.slice.call('hello',1,3),'el');console.log('PASS');
