function check(a,b){if(a!==b)throw a+" !== "+b;} check(/a+/.test('aaa'),true);check('baaac'.match(/a+/)[0],'aaa');check(/a+/.test('b')===false,true);console.log('PASS');
