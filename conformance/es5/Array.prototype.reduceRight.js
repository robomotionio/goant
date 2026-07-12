function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3].reduceRight(function(a,b){return a+'-'+b;}),'3-2-1');console.log('PASS');
