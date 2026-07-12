function check(a,b){if(a!==b)throw a+" !== "+b;} check(/[\b]/.test(String.fromCharCode(8)),true);console.log('PASS');
