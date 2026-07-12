function check(a,b){if(a!==b)throw a+" !== "+b;} check(/x/m.multiline,true);check(/x/.multiline,false);console.log('PASS');
