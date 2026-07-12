function check(a,b){if(a!==b)throw a+" !== "+b;} var a=1,b=2,c=1;check(a/b/c,0.5);check(4/2/1,2);console.log('PASS');
