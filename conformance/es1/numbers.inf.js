function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} check(Infinity+1,Infinity);check(Infinity-Infinity!==Infinity-Infinity,true);check(1/Infinity,0);console.log('PASS');
