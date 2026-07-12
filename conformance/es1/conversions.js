function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(1+'2','12');check('3'*2,6);check(true+1,2);check([]+[],'');check([1]+[2],'12');console.log('PASS');
