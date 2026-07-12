function check(a,b){if(a!==b)throw a+" !== "+b;} check(({}).hasOwnProperty!==undefined,true);var o={a:1};check(o.hasOwnProperty('a'),true);console.log('PASS');
