function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3,2].lastIndexOf(2),3);check([1,2,3].lastIndexOf(5),-1);console.log('PASS');
