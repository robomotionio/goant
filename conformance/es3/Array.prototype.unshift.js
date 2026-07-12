function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[3,4];check(a.unshift(1,2),4);check(a.join(','),'1,2,3,4');console.log('PASS');
