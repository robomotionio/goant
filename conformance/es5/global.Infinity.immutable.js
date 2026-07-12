function check(a,b){if(a!==b)throw a+" !== "+b;} Infinity=5;check(1/0,Infinity);console.log('PASS');
