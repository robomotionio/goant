function check(a,b){if(a!==b)throw a+" !== "+b;} check('aaaa'.match(/a{2,3}/)[0],'aaa');check('a'.match(/a{2,3}/),null);console.log('PASS');
