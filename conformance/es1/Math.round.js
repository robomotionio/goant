function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Math.round(2.5),3);check(Math.round(2.4),2);check(Math.round(-2.5),-2);console.log('PASS');
