function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var x=5;check(x,5);var y;check(y,undefined);var a=1,b=2;check(a+b,3);console.log('PASS');
