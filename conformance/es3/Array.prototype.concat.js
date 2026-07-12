function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3].concat([4,5]).join(','),'1,2,3,4,5');check([1].concat(2,3).join(','),'1,2,3');console.log('PASS');
