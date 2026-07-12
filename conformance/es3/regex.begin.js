function check(a,b){if(a!==b)throw a+" !== "+b;} check(/^abc/.test('abcdef'),true);check(/^abc/.test('xabc'),false);console.log('PASS');
