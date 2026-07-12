function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.exp(0),1);check(Math.exp(1)>2.71&&Math.exp(1)<2.72,true);console.log('PASS');
