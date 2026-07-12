function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setMinutes(45);check(d.getUTCMinutes(),45);console.log('PASS');
