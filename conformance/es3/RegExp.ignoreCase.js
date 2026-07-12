function check(a,b){if(a!==b)throw a+" !== "+b;} check(/x/i.ignoreCase,true);check(/x/.ignoreCase,false);console.log('PASS');
