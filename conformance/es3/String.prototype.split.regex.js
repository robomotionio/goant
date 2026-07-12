function check(a,b){if(a!==b)throw a+" !== "+b;} check('a,b,c'.split(/,/).join('|'),'a|b|c');check('a1b2c'.split(/\d/).join('-'),'a-b-c');console.log('PASS');
