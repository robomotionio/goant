function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof TypeError,'function');check(new TypeError('x') instanceof Error,true);console.log('PASS');
