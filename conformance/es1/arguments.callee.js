function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} function f(){return arguments.callee;}check(f()===f,true);console.log('PASS');
