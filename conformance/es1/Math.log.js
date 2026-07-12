function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.log(1),0);check(Math.log(Math.E)>0.99&&Math.log(Math.E)<1.01,true);console.log('PASS');
