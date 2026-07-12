function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setUTCMilliseconds(100);check(d.getUTCMilliseconds(),100);console.log('PASS');
