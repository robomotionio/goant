function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof JSON,'object');check(typeof JSON.parse,'function');check(typeof JSON.stringify,'function');console.log('PASS');
