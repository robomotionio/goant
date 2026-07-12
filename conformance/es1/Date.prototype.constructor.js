function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(new Date(0).constructor,Date);console.log('PASS');
