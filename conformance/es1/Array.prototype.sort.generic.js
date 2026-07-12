function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var a=[3,1,2];check(a.sort().join(','),'1,2,3');console.log('PASS');
