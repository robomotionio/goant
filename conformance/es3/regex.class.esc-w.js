function check(a,b){if(a!==b)throw a+" !== "+b;} check('a_b'.match(/\w+/)[0],'a_b');check(/\w/.test('!')===false,true);console.log('PASS');
