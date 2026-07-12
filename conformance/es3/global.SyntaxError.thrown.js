function check(a,b){if(a!==b)throw a+" !== "+b;} var c=false;try{eval('"unterminated');}catch(e){c=e instanceof SyntaxError;}check(c,true);console.log('PASS');
