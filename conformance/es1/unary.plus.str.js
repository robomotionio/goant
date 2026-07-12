function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(+'3.14',3.14);check(+'  10  ',10);check(+'0x1F',31);console.log('PASS');
