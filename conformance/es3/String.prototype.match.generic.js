function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.prototype.match.call('abc',/b/)[0],'b');console.log('PASS');
