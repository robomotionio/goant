function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var s=new Date(0).toString();check(typeof s,'string');check(s.length>0,true);console.log('PASS');
