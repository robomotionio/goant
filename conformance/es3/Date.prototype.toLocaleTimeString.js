function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof new Date(0).toLocaleTimeString(),'string');console.log('PASS');
