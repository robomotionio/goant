function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.prototype.concat.call('a','b'),'ab');console.log('PASS');
