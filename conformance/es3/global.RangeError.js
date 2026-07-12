function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof RangeError,'function');check(new RangeError() instanceof Error,true);console.log('PASS');
