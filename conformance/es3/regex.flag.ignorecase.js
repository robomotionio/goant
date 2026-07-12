function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc/i.test('ABC'),true);check(/abc/i.test('AbC'),true);check(/abc/.test('ABC'),false);console.log('PASS');
