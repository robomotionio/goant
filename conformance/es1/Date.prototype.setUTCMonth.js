function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setUTCMonth(3);check(d.getUTCMonth(),3);console.log('PASS');
