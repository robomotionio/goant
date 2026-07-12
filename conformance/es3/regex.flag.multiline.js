function check(a,b){if(a!==b)throw a+" !== "+b;} check(/^b/m.test('a\nb'),true);check(/^b/.test('a\nb'),false);console.log('PASS');
