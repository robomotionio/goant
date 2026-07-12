function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(parseInt('0xFF'),255);check(parseInt('ff',16),255);check(parseInt('10',16),16);console.log('PASS');
