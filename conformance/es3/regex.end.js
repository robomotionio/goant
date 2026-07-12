function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc$/.test('xabc'),true);check(/abc$/.test('abcx'),false);console.log('PASS');
