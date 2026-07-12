function check(a,b){if(a!==b)throw a+" !== "+b;} check(/foo(?=bar)/.test('foobar'),true);check(/foo(?=bar)/.test('foobaz'),false);check('foobar'.match(/foo(?=bar)/)[0],'foo');console.log('PASS');
