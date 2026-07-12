function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3,4].filter(function(x){return x%2===0;}).join(','),'2,4');console.log('PASS');
