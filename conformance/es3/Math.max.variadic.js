function check(a,b){if(a!==b)throw a+" !== "+b;} check(Math.max(1,2,3,4,5),5);check(Math.max.apply(null,[1,9,2]),9);console.log('PASS');
