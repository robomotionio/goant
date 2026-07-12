function check(a,b){if(a!==b)throw a+" !== "+b;} check(/a|b/.test('b'),true);check(/cat|dog/.test('dog'),true);check('dog'.match(/cat|dog/)[0],'dog');console.log('PASS');
