function check(a,b){if(a!==b)throw a+" !== "+b;} check(typeof EvalError,'function');check(new EvalError() instanceof Error,true);console.log('PASS');
