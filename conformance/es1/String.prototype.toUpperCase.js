function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check('hello'.toUpperCase(),'HELLO');check('MiXeD'.toUpperCase(),'MIXED');console.log('PASS');
