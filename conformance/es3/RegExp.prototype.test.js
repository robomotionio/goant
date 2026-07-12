function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc/.test('xabcy'),true);check(/abc/.test('xyz'),false);console.log('PASS');
