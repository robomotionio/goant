function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var a=[1,2,3,4,5];a.length=3;check(a.length,3);check(a[3],undefined);a.length=0;check(a.length,0);console.log('PASS');
