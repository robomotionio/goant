function check(a,b){if(a!==b)throw a+" !== "+b;} check((1.005).toExponential(2),'1.00e+0');check((9.999).toExponential(2),'1.00e+1');console.log('PASS');
