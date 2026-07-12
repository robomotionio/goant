function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof Date.now(),'number');check(Date.now()>0,true);console.log('PASS');
