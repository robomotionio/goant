function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof SyntaxError,'function');check(new SyntaxError() instanceof Error,true);console.log('PASS');
