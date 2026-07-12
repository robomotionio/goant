function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.fromCharCode(0).match(/\0/)[0],String.fromCharCode(0));console.log('PASS');
