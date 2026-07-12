function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(unescape('a%20b'),'a b');check(unescape('%21'),'!');console.log('PASS');
