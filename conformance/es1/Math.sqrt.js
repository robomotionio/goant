function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.sqrt(16),4);check(Math.sqrt(0),0);check(Math.sqrt(2)>1.41&&Math.sqrt(2)<1.42,true);console.log('PASS');
