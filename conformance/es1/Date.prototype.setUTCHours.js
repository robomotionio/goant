function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setUTCHours(6);check(d.getUTCHours(),6);console.log('PASS');
