function check(a,b){if(a!==b)throw a+" !== "+b;} check(Math.min(5,4,3,2,1),1);check(Math.min.apply(null,[3,1,2]),1);console.log('PASS');
