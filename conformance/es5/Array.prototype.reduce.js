function check(a,b){if(a!==b)throw a+" !== "+b;} check([1,2,3,4].reduce(function(a,b){return a+b;}),10);check([1,2,3].reduce(function(a,b){return a+b;},10),16);console.log('PASS');
