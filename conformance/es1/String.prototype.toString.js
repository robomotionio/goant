function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check('abc'.toString(),'abc');check(('x').toString(),'x');console.log('PASS');
