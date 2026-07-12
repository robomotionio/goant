function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(delete undeclaredXYZ,true);console.log('PASS');
