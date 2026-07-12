function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,,3];check(a.length,3);check(a[1],undefined);console.log('PASS');
