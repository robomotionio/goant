function check(a,b){if(a!==b)throw a+" !== "+b;} check(/[abc]/.test('b'),true);check(/[abc]/.test('d'),false);check('xby'.match(/[abc]/)[0],'b');console.log('PASS');
