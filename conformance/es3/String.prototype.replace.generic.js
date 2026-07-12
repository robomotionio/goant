function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.prototype.replace.call('hello','l','L'),'heLlo');console.log('PASS');
