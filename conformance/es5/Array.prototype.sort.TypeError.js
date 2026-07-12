function check(a,b){if(a!==b)throw a+" !== "+b;} var t=false;try{[1,2].sort(5);}catch(e){t=true;}check([3,1,2].sort().join(','),'1,2,3');console.log('PASS');
