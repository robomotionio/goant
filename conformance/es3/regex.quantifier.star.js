function check(a,b){if(a!==b)throw a+" !== "+b;} check(/a*/.test(''),true);check('baaac'.match(/a*/)[0],'');check('aaa'.match(/a*/)[0],'aaa');console.log('PASS');
