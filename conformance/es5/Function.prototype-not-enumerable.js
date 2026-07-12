function check(a,b){if(a!==b)throw a+" !== "+b;} var obj={};check(typeof obj.toString,'function');console.log('PASS');
