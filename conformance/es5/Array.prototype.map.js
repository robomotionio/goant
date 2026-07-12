function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3].map(function(x){return x*x;}).join(','),'1,4,9');console.log('PASS');
