function check(a,b){if(a!==b)throw a+" !== "+b;} check(/[a-z]/.test('m'),true);check(/[a-z]/.test('5'),false);check(/[0-9]/.test('7'),true);console.log('PASS');
