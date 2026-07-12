function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(0.1+0.2!==0.3,true);check(1.5*2,3);check(10/4,2.5);console.log('PASS');
