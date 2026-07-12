function check(a,b){if(a!==b)throw a+" !== "+b;} check('hello'.substr(1,3),'ell');check('hello'.substr(-2),'lo');check('hello'.substr(0,2),'he');console.log('PASS');
