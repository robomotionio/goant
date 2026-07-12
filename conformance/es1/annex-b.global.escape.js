function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(escape('a b'),'a%20b');check(escape('!'),'%21');console.log('PASS');
