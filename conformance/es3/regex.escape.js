function check(a,b){if(a!==b)throw a+" !== "+b;} check('a.b'.match(/\./)[0],'.');check('a*b'.match(/\*/)[0],'*');check('a+b'.match(/\+/)[0],'+');console.log('PASS');
