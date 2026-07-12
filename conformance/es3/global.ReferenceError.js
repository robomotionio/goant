function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof ReferenceError,'function');check(new ReferenceError() instanceof Error,true);console.log('PASS');
