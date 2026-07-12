function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check('é'.charCodeAt(0),233);check('A'.charCodeAt(0),65);console.log('PASS');
