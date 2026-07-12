function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3,2].indexOf(2),1);check([1,2,3].indexOf(5),-1);console.log('PASS');
