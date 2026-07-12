function check(a,b){if(a!==b)throw a+" !== "+b;} check('abc'.concat('def'),'abcdef');check('a'.concat('b','c'),'abc');console.log('PASS');
