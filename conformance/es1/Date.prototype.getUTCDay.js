function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(new Date(Date.UTC(2020,0,1)).getUTCDay(),3);console.log('PASS');
