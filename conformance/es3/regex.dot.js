function check(a,b){if(a!==b)throw a+" !== "+b;} check(/a.c/.test('abc'),true);check(/a.c/.test('a c'),true);check(/a.c/.test('ac'),false);console.log('PASS');
