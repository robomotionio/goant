function check(a,b){if(a!==b)throw a+" !== "+b;} function outer(){function inner(){return 5;}return inner();}check(outer(),5);console.log('PASS');
