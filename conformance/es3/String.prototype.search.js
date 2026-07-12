function check(a,b){if(a!==b)throw a+" !== "+b;} check('hello'.search(/l/),2);check('hello'.search(/z/),-1);console.log('PASS');
