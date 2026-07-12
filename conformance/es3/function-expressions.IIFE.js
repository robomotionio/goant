function check(a,b){if(a!==b)throw a+" !== "+b;} var r=(function(x){return x*2;})(21);check(r,42);console.log('PASS');
