function check(a,b){if(a!==b)throw a+" !== "+b;} check('A'.match(/\x41/)[0],'A');check(String.fromCharCode(9).match(/\x09/)[0],String.fromCharCode(9));console.log('PASS');
