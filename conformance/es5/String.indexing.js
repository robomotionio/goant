function check(a,b){if(a!==b)throw a+" !== "+b;} check('hello'[0],'h');check('hello'[4],'o');check('hello'[10],undefined);console.log('PASS');
