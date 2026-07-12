function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setUTCDate(20);check(d.getUTCDate(),20);console.log('PASS');
