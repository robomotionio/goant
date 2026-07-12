function check(a,b){if(a!==b)throw a+" !== "+b;} var a=[1,2,3];check(a.length,3);check(a[0],1);var b=[];check(b.length,0);console.log('PASS');
