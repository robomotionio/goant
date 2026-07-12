function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setMonth(5);check(d.getUTCMonth(),5);console.log('PASS');
