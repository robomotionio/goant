function check(a,b){if(a!==b)throw a+" !== "+b;} var e=new Error('x');check(e.constructor,Error);check(Error.prototype.constructor,Error);console.log('PASS');
