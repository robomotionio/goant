function check(a,b){if(a!==b)throw a+" !== "+b;} check('aaaa'.match(/a{2}/)[0],'aa');check(/a{3}/.test('aa')===false,true);console.log('PASS');
