function check(a,b){if(a!==b)throw a+" !== "+b;} check(/x/g.global,true);check(/x/.global,false);console.log('PASS');
