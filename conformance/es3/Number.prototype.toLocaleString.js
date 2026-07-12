function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof (1234.5).toLocaleString(),'string');check((5).toLocaleString(),'5');check((0).toLocaleString(),'0');console.log('PASS');
