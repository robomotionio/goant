function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(typeof undeclaredXYZ,'undefined');console.log('PASS');
