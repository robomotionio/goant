function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Infinity>1e308,true);check(1/0,Infinity);check(-1/0,-Infinity);console.log('PASS');
