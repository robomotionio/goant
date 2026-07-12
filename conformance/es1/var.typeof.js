function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var x=5;check(typeof x,'number');var y;check(typeof y,'undefined');console.log('PASS');
