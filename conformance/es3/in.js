function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1};check('a' in o,true);check('b' in o,false);check(0 in [1],true);console.log('PASS');
