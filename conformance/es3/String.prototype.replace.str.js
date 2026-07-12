function check(a,b){if(a!==b)throw a+" !== "+b;} check('hello'.replace('l','L'),'heLlo');check('a.b'.replace('.','_'),'a_b');console.log('PASS');
