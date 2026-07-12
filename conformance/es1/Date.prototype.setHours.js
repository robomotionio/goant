function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setHours(13);check(d.getUTCHours(),13);console.log('PASS');
