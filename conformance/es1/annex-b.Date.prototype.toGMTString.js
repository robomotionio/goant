function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(new Date(0).toGMTString(),'Thu, 01 Jan 1970 00:00:00 GMT');console.log('PASS');
