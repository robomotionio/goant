function check(a,b){if(a!==b)throw a+" !== "+b;} check(/foo(?!bar)/.test('foobaz'),true);check(/foo(?!bar)/.test('foobar'),false);console.log('PASS');
