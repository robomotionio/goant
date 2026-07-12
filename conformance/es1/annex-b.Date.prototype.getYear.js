function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(new Date(Date.UTC(1970,0,1)).getYear(),70);check(new Date(Date.UTC(2000,0,1)).getYear(),100);console.log('PASS');
