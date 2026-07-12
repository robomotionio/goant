function check(a,b){if(a!==b)throw a+" !== "+b;} var c=false;try{null.x;}catch(e){c=e instanceof TypeError;}check(c,true);console.log('PASS');
