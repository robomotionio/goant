function check(a,b){if(a!==b)throw a+" !== "+b;} check(/(?:)/.test(''),true);check(new RegExp('').test('anything'),true);console.log('PASS');
