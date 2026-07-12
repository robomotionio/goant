function check(a,b){if(a!==b)throw a+" !== "+b;} check('a,b,c,d'.split(',',2).length,2);check('a,b,c'.split(',',2).join('|'),'a|b');console.log('PASS');
