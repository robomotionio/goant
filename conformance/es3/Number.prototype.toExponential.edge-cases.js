function check(a,b){if(a!==b)throw a+" !== "+b;} check((0).toExponential(2),'0.00e+0');check((1000000).toExponential(),'1e+6');console.log('PASS');
