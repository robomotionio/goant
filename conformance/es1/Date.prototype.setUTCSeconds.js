function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setUTCSeconds(45);check(d.getUTCSeconds(),45);console.log('PASS');
