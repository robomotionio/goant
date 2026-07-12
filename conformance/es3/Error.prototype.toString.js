function check(a,b){if(a!==b)throw a+" !== "+b;} check(new Error('boom').toString(),'Error: boom');check(new Error().toString(),'Error');console.log('PASS');
