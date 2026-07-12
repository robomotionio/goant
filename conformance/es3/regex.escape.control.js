function check(a,b){if(a!==b)throw a+" !== "+b;} check(String.fromCharCode(1).match(/\cA/)[0],String.fromCharCode(1));console.log('PASS');
