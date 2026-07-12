function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2];check(a.push(3),3);check(a.join(','),'1,2,3');check(a.push(4,5),5);console.log('PASS');
