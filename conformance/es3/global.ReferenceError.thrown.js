function check(a,b){if(a!==b)throw a+" !== "+b;} check(new ReferenceError('x').name,'ReferenceError');console.log('PASS');
