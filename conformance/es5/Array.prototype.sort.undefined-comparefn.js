function check(a,b){if(a!==b)throw a+" !== "+b;} check([3,1,2].sort().join(','),'1,2,3');check([3,1,2].sort(function(a,b){return b-a;}).join(','),'3,2,1');console.log('PASS');
