function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var o={a:1};check(delete o.a,true);check('a' in o,false);console.log('PASS');
