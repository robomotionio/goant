function check(a,b){if(a!==b)throw a+" !== "+b;} check(new Error().name,'Error');check(new TypeError().name,'TypeError');console.log('PASS');
