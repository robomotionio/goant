function check(a,b){if(a!==b)throw a+" !== "+b;} check('abc'.slice(1),'bc');check('abc'.slice(-2),'bc');check('abc'.slice(0,2),'ab');console.log('PASS');
