function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var d=new Date(0);d.setYear(99);check(d.getUTCFullYear(),1999);d.setYear(2025);check(d.getUTCFullYear(),2025);console.log('PASS');
