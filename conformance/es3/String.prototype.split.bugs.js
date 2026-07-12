function check(a,b){if(a!==b)throw a+" !== "+b;} check(''.split('').length,0);check('a'.split('').length,1);check('abc'.split('').join('-'),'a-b-c');console.log('PASS');
