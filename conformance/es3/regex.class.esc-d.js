function check(a,b){if(a!==b)throw a+" !== "+b;} check('a1b2'.match(/\d/)[0],'1');check(/\d+/.test('123'),true);check(/\d/.test('abc'),false);console.log('PASS');
