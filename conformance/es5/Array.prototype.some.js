function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3].some(function(x){return x>2;}),true);check([1,2,3].some(function(x){return x>5;}),false);console.log('PASS');
