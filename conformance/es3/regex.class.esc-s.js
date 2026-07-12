function check(a,b){if(a!==b)throw a+" !== "+b;} check('a b'.match(/\s/)[0],' ');check(/\s/.test('nospace')===false,true);console.log('PASS');
