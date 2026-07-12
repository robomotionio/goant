function check(a,b){if(a!==b)throw a+" !== "+b;} var c=false;try{(1).toString(99);}catch(e){c=e instanceof RangeError;}check(c,true);console.log('PASS');
