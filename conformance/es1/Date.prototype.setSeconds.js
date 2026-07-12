function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setSeconds(30);check(d.getUTCSeconds(),30);console.log('PASS');
