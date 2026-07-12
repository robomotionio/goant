function check(a,b){if(a!==b)throw a+" !== "+b;} check(/(?:abc)+/.test('abcabc'),true);check('abcabc'.match(/(?:abc)+/)[0],'abcabc');check('x'.match(/(?:a)/),null);console.log('PASS');
