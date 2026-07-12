function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} function foo(a,b){return a+b;}var s=foo.toString();check(typeof s,'string');check(s.indexOf('foo')>=0,true);console.log('PASS');
