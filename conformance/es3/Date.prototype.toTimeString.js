function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof new Date(0).toTimeString(),'string');console.log('PASS');
