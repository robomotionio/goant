function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2,3,4];check(a.slice(1,3).join(','),'2,3');check(a.slice(-2).join(','),'3,4');console.log('PASS');
