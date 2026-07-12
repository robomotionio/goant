function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var s=new Date(0).toLocaleString();check(typeof s,'string');console.log('PASS');
