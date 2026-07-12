function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3].every(function(x){return x>0;}),true);check([1,-2,3].every(function(x){return x>0;}),false);console.log('PASS');
