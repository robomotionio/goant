function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2,3,4,5];check(a.splice(1,2).join(','),'2,3');check(a.join(','),'1,4,5');console.log('PASS');
