function check(a,b){if(a!==b)throw a+" !== "+b;} check((123.456).toExponential(2),'1.23e+2');check((0.001).toExponential(1),'1.0e-3');console.log('PASS');
