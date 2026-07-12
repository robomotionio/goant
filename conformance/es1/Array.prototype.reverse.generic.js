function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var o={0:1,1:2,2:3,length:3};Array.prototype.reverse.call(o);check(o[0],3);check(o[2],1);console.log('PASS');
