function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2,3];check(a.shift(),1);check(a.join(','),'2,3');console.log('PASS');
