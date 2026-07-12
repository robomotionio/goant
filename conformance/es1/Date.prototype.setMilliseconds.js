function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setMilliseconds(250);check(d.getUTCMilliseconds(),250);console.log('PASS');
