function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setDate(15);check(d.getUTCDate(),15);console.log('PASS');
