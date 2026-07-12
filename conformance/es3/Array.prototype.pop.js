function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2,3];check(a.pop(),3);check(a.length,2);check([].pop(),undefined);console.log('PASS');
