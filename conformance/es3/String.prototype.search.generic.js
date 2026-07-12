function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.prototype.search.call('hello',/o/),4);console.log('PASS');
