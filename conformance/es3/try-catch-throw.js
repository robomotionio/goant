function check(a,b){if(a!==b)throw a+" !== "+b;} var r=0;try{throw 42;}catch(e){r=e;}check(r,42);console.log('PASS');
